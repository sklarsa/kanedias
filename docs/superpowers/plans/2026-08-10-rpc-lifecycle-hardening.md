# RPC Lifecycle Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Repeatedly prove and harden local-model child spawn, natural settlement, stop, root end, interrupt, steer, and mixed parallel lifecycle behavior through the real Pi RPC, supervisor, proxy, and Incus paths.

**Architecture:** Add an Incus-gated lifecycle acceptance suite beside the existing live supervisor harness. Reuse its real provisioning and cleanup machinery, add a non-destructive event journal and asserted asynchronous child-call helper, then exercise deterministic API and model-directed variants. Confirm any observed defect with a narrow failing regression before modifying production code; the final gate is five independent clean executions of every lifecycle scenario.

**Tech Stack:** Go 1.24, Pi JSON-line RPC, supervisor Unix HTTP/SSE APIs, the Kanedias Pi extension and `delegate_session`, Incus, the local OpenAI-compatible `local-executor` provider, Qwen3.6-27B-GGUF, and goproxy.

## Global Constraints

- Run only in `/home/steven/source/github/kanedias/.worktrees/rpc-lifecycle-hardening`; keep the main checkout untouched.
- Use the authorized environment from `/home/steven/source/github/kanedias/.env` without copying or committing it.
- The lifecycle suite must use provider `local-executor` and model `Qwen3.6-27B-GGUF` for the root and every worker.
- A failed simple local-model `delegate_session` request is an acceptance failure and must be investigated; do not dismiss it without provider, Pi, and supervisor evidence.
- Do not add blind sleeps or retries. Poll only observable states with bounded deadlines.
- Preserve every accepted event in order. Genuine prolonged backpressure remains fatal after 4,096 events or 16 MiB.
- No log or artifact may expose credentials; proxy warnings must not expose hosts, paths, request data, or raw errors.
- Each scenario must return to its exact pre-run Incus instance and volume baseline and leave no owned process or Unix socket.
- After any code change or failed run, reset the affected scenario's clean-run count to zero.
- Any production fix must follow systematic debugging and TDD: first divergent boundary, failing regression, minimal fix, focused race test, then live rerun.

---

## File Structure

- Create `internal/supervisor/live_rpc_lifecycle_support_test.go`: Incus-gated local-model configuration validation, event journaling, asynchronous child-call results, scenario setup, artifact capture, and shared lifecycle assertions.
- Create `internal/supervisor/live_rpc_lifecycle_test.go`: the eight live lifecycle scenarios and their deterministic/model-directed actions.
- Modify a production file only after a live failure proves the responsible boundary. At that point add the exact file and regression test to this plan before editing implementation code.
- Do not enlarge `internal/supervisor/live_incus_test.go` except for a small reusable helper that cannot reasonably remain in the lifecycle support file.

### Shared test interfaces

The two new files use these private test-only interfaces:

```go
type lifecycleHTTPResult struct {
    Status int
    Body   []byte
    Err    error
}

type lifecycleChildCall struct {
    label string
    done  chan lifecycleHTTPResult
}

type lifecycleEventJournal struct {
    mu     sync.Mutex
    events []supervisor.EventEnvelope
    done   chan struct{}
}

type lifecycleRoot struct {
    process *acceptanceProcess
    socket  string
    tree    supervisor.NodeSnapshot
    client  *http.Client
    stream  *sseCapture
    journal *lifecycleEventJournal
    stalled net.Conn
}

func validateLifecycleModelPolicy(cfg config.Config, provider, model string) error
func (h *liveAcceptance) prepareLifecycleConfig()
func (h *liveAcceptance) startLifecycleRoot(label string) *lifecycleRoot
func (h *liveAcceptance) startLifecycleChildCall(client *http.Client, parentID, label, task string) *lifecycleChildCall
func (call *lifecycleChildCall) wait(t *testing.T, timeout time.Duration) lifecycleHTTPResult
func newLifecycleEventJournal(stream *sseCapture) *lifecycleEventJournal
func (journal *lifecycleEventJournal) snapshot() []supervisor.EventEnvelope
func (journal *lifecycleEventJournal) countPi(sessionID, eventType, toolName string) int
func (h *liveAcceptance) stopLifecycleRoot(root *lifecycleRoot)
func (h *liveAcceptance) assertRootUsable(root *lifecycleRoot, marker string)
func (h *liveAcceptance) assertLifecycleProxyQuiet()
func runLifecycleScenario(t *testing.T, label string, run func(*liveAcceptance))
```

---

### Task 1: Build the local lifecycle harness and evidence journal

**Files:**
- Create: `internal/supervisor/live_rpc_lifecycle_support_test.go`
- Test: `internal/supervisor/live_rpc_lifecycle_support_test.go`
- Reuse: `internal/supervisor/live_incus_test.go:343-412,470-833,1199-1312,1313-1788,2010-2043,2175-2280`

**Interfaces:**
- Consumes: `newLiveAcceptance`, `writeManagedConfig`, `startRoot`, `unixRequest`, `unixJSON`, `liveAcceptance.writeJSON`, `liveAcceptance.assertBaseline`, `liveAcceptance.stopProxy`, and `supervisor.EventEnvelope`.
- Produces: every shared test interface listed above.

- [ ] **Step 1: Write failing unit tests for model-policy validation and event journaling**

Add the Incus build constraint and package declaration, then tests which require the root and all configured workers to resolve to one provider/model and prove journal snapshots are ordered copies rather than destructive reads:

```go
//go:build incus

package supervisor_test

func TestValidateLifecycleModelPolicyRequiresLocalRootAndWorkers(t *testing.T) {
    cfg := config.Config{
        Models: map[string]config.ModelDefinition{
            "local": {Provider: "local-executor", Model: "Qwen3.6-27B-GGUF", ThinkingLevels: []string{"off"}, DefaultThinkingLevel: "off"},
            "paid":  {Provider: "openai-codex", Model: "gpt-5.6-sol", ThinkingLevels: []string{"high"}, DefaultThinkingLevel: "high"},
        },
        Session: config.SessionDefaults{ModelType: "local", ThinkingLevel: "off"},
        Workers: map[string]config.WorkerDefaults{
            "reviewer": {Description: "review", ModelType: "local", ThinkingLevel: "off"},
        },
    }
    if err := validateLifecycleModelPolicy(cfg, "local-executor", "Qwen3.6-27B-GGUF"); err != nil {
        t.Fatalf("valid local policy: %v", err)
    }
    worker := cfg.Workers["reviewer"]
    worker.ModelType = "paid"
    cfg.Workers["reviewer"] = worker
    if err := validateLifecycleModelPolicy(cfg, "local-executor", "Qwen3.6-27B-GGUF"); err == nil || !strings.Contains(err.Error(), "reviewer") {
        t.Fatalf("mixed worker policy error = %v", err)
    }
}

func TestLifecycleEventJournalPreservesOrderAndSupportsRepeatedQueries(t *testing.T) {
    input := make(chan supervisor.EventEnvelope, 3)
    stream := &sseCapture{events: input}
    journal := newLifecycleEventJournal(stream)
    input <- supervisor.EventEnvelope{Seq: 1, SessionID: "root", Kind: "pi", Payload: json.RawMessage(`{"type":"agent_start"}`)}
    input <- supervisor.EventEnvelope{Seq: 2, SessionID: "root", Kind: "pi", Payload: json.RawMessage(`{"type":"tool_execution_start","toolName":"delegate_session"}`)}
    close(input)
    <-journal.done

    first := journal.snapshot()
    second := journal.snapshot()
    if len(first) != 2 || len(second) != 2 || first[0].Seq != 1 || first[1].Seq != 2 {
        t.Fatalf("journal snapshots = %#v / %#v", first, second)
    }
    first[0].SessionID = "mutated"
    if journal.snapshot()[0].SessionID != "root" {
        t.Fatal("snapshot aliases retained journal state")
    }
    if got := journal.countPi("root", "tool_execution_start", "delegate_session"); got != 1 {
        t.Fatalf("delegate_session starts = %d, want 1", got)
    }
}
```

These literals use the exported definitions in `internal/config/model_policy.go`.

- [ ] **Step 2: Run the support tests and confirm the intended compile failure**

Run:

```bash
go test -tags=incus ./internal/supervisor -run 'TestValidateLifecycleModelPolicy|TestLifecycleEventJournal' -count=1
```

Expected: FAIL because `validateLifecycleModelPolicy` and `newLifecycleEventJournal` do not exist.

- [ ] **Step 3: Implement policy validation and the non-destructive journal**

Implement `validateLifecycleModelPolicy` by calling `cfg.DefaultSessionModelPolicy()`, checking `policy.Root`, and checking each sorted worker name. Include the worker name in errors. Implement the journal as the sole consumer of `stream.events`, append cloned envelopes under a mutex, close `done` when the stream closes, and return deep copies from `snapshot`.

Payload parsing for `countPi` must use only `type` and `toolName`:

```go
func (journal *lifecycleEventJournal) countPi(sessionID, eventType, toolName string) int {
    count := 0
    for _, event := range journal.snapshot() {
        if event.SessionID != sessionID || event.Kind != "pi" {
            continue
        }
        var payload struct {
            Type     string `json:"type"`
            ToolName string `json:"toolName"`
        }
        if json.Unmarshal(event.Payload, &payload) == nil && payload.Type == eventType && (toolName == "" || payload.ToolName == toolName) {
            count++
        }
    }
    return count
}
```

- [ ] **Step 4: Implement scenario setup and asserted asynchronous calls**

`prepareLifecycleConfig` must:

1. Require `KANEDIAS_E2E_WORKER_PROVIDER=local-executor` and `KANEDIAS_E2E_WORKER_MODEL=Qwen3.6-27B-GGUF`.
2. Create run-local managed socket/log directories.
3. Call `writeManagedConfig` before replacing `h.configPath`.
4. Load and validate the generated config.
5. Assign both `h.configPath` and `h.cfg` to the generated values.
6. Call `validateLifecycleModelPolicy` for the exact local provider/model.

`startLifecycleChildCall` must POST this request in an `h.async`-tracked goroutine and return the result through a buffered channel:

```go
request := map[string]any{
    "workerType": "reviewer",
    "kind":       "read",
    "context":    "fresh",
    "task":       task,
}
status, body, err := unixRequest(client, http.MethodPost,
    "/v1/sessions/"+url.PathEscape(parentID)+"/children", request)
```

Always write a JSON-safe result artifact containing status, body text, and `errorString(err)`. `wait` must fail on timeout instead of returning a zero result.

`runLifecycleScenario` must call `requireLiveSupervisorAuthorization` before any side effect, construct and defer-close a fresh `liveAcceptance`, prepare the local config, build the reviewed checkout, start the owned proxy, invoke the scenario, stop the proxy, inspect proxy logs, assert the exact baseline, and set `h.success = true` only after every assertion passes.

- [ ] **Step 5: Implement shared root and teardown assertions**

`startLifecycleRoot` wraps `startRoot`, starts the event journal immediately, and retains the deliberately stalled SSE connection for explicit close. `stopLifecycleRoot` must issue `DELETE /v1/sessions/{rootID}`, require HTTP 202, wait for the process, close the stalled connection, verify the root socket is absent, and poll until every tracked tree session is absent.

`assertRootUsable` sends a short prompt with a unique marker, waits for a new root `agent_settled` count, requires the marker in `get_last_assistant_text`, then requires successful `get_state` with `data.isStreaming == false`.

`assertLifecycleProxyQuiet` reads `proxy.log` after the owned proxy exits and fails if it contains `proxy internal warning`, `pi RPC event consumer exceeded bounded capacity`, or any configured remote credential substring. Do not render raw diagnostic arguments in failure output; identify only the matched safe class/string.

- [ ] **Step 6: Run, race, format, and commit the support layer**

Run:

```bash
gofmt -w internal/supervisor/live_rpc_lifecycle_support_test.go
go test -tags=incus ./internal/supervisor -run 'TestValidateLifecycleModelPolicy|TestLifecycleEventJournal' -count=1
go test -race -tags=incus ./internal/supervisor -run 'TestValidateLifecycleModelPolicy|TestLifecycleEventJournal' -count=1
git diff --check
git add internal/supervisor/live_rpc_lifecycle_support_test.go
git commit -m "test: add RPC lifecycle acceptance harness"
```

Expected: support tests PASS under the race detector; no live side effect occurs because no `TestLive` function is selected.

---

### Task 2: Add deterministic single, parallel, child-stop, and root-end scenarios

**Files:**
- Create: `internal/supervisor/live_rpc_lifecycle_test.go`
- Modify: `internal/supervisor/live_rpc_lifecycle_support_test.go` only if a generally reusable assertion is missing
- Test: `internal/supervisor/live_rpc_lifecycle_test.go`

**Interfaces:**
- Consumes: all Task 1 shared test interfaces.
- Produces: `TestLiveRPCDeterministicChildLifecycle`, `TestLiveRPCChildStopLifecycle`, and `TestLiveRPCRootEndLifecycle`.

- [ ] **Step 1: Write the deterministic live tests before scenario methods**

Add:

```go
//go:build incus

package supervisor_test

func TestLiveRPCDeterministicChildLifecycle(t *testing.T) {
    runLifecycleScenario(t, "deterministic-children", func(h *liveAcceptance) {
        h.exerciseDeterministicChildren()
    })
}

func TestLiveRPCChildStopLifecycle(t *testing.T) {
    runLifecycleScenario(t, "child-stop", func(h *liveAcceptance) {
        h.exerciseLifecycleChildStop()
    })
}

func TestLiveRPCRootEndLifecycle(t *testing.T) {
    runLifecycleScenario(t, "root-end", func(h *liveAcceptance) {
        h.exerciseLifecycleRootEnd()
    })
}
```

- [ ] **Step 2: Compile without authorization and verify missing methods fail**

Run:

```bash
go test -tags=incus ./internal/supervisor -run '^TestLiveRPC(DeterministicChild|ChildStop|RootEnd)Lifecycle$' -count=1
```

Expected: FAIL to compile because the three exercise methods do not exist. The live authorization gate must not be bypassed once compilation succeeds.

- [ ] **Step 3: Implement deterministic single and parallel natural completion**

`exerciseDeterministicChildren` must:

1. Start one lifecycle root and one short child call with marker `KANEDIAS_LIFECYCLE_DIRECT_SINGLE_<prefix>`.
2. Observe one bound child in the tree, track its identity/resources, and require an HTTP 200 terminal read result containing the marker.
3. Wait for the child to disappear and prove the root remains usable.
4. Start three calls concurrently with distinct `DIRECT_PARALLEL_0..2` markers and repository-read tasks.
5. Poll until one tree snapshot contains all three children simultaneously; track and validate distinct identities.
6. Require all three HTTP 200 results and markers, child disappearance, and a usable root.
7. Stop the root and return to baseline.

Do not call the existing destructive `waitPiEvent` helper after starting the journal; all event assertions must query the journal.

- [ ] **Step 4: Implement active child stop**

Use a task that requests a response long enough to observe `LifecycleRunning`. Wait for the child snapshot and at least one child `agent_start`, issue `DELETE /v1/sessions/{childID}`, require HTTP 202, and require the pending child call to settle within two minutes with a typed JSON error rather than hanging or returning an invalid success. Record the observed canonical error code in the artifact.

Then require child resource/socket disappearance and call `assertRootUsable`. If the observed error is inconsistent across identical runs, treat that as the first defect rather than broadening the assertion.

- [ ] **Step 5: Implement graceful root end with three active children**

Start three long child calls concurrently, wait for all three in one tree snapshot, track the full tree and descendant PIDs, then issue root DELETE. Require HTTP 202, root process exit, all three pending calls to settle, every process/socket to disappear, and all four sessions' Incus resources to return to baseline. The root socket must reject later requests because it no longer exists.

- [ ] **Step 6: Compile, format, and commit deterministic scenarios**

Run the non-live package compile and support tests:

```bash
gofmt -w internal/supervisor/live_rpc_lifecycle_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
go test -tags=incus ./internal/supervisor -run 'TestValidateLifecycleModelPolicy|TestLifecycleEventJournal' -count=1
git diff --check
git add internal/supervisor/live_rpc_lifecycle_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
git commit -m "test: cover deterministic RPC lifecycle controls"
```

Expected: compilation and support tests PASS. Do not claim the live scenarios pass until Task 6 executes them with authorization.

---

### Task 3: Add local-model single and parallel `delegate_session` scenarios

**Files:**
- Modify: `internal/supervisor/live_rpc_lifecycle_test.go`
- Modify: `internal/supervisor/live_rpc_lifecycle_support_test.go` only for reusable model-event assertions
- Test: `internal/supervisor/live_rpc_lifecycle_test.go`

**Interfaces:**
- Consumes: Task 1 journal, root helpers, and Task 2 tree/identity assertions.
- Produces: `TestLiveRPCModelChildLifecycle` and exact local-model tool-call acceptance.

- [ ] **Step 1: Write the model-directed test entry point**

```go
func TestLiveRPCModelChildLifecycle(t *testing.T) {
    runLifecycleScenario(t, "model-children", func(h *liveAcceptance) {
        h.exerciseModelDirectedChildren()
    })
}
```

- [ ] **Step 2: Verify the missing implementation fails compilation**

Run:

```bash
go test -tags=incus ./internal/supervisor -run '^TestLiveRPCModelChildLifecycle$' -count=1
```

Expected: FAIL because `exerciseModelDirectedChildren` is undefined.

- [ ] **Step 3: Implement exact single-tool acceptance**

Start a fresh root and send this shape of prompt, with a run-unique marker:

```text
In your next assistant turn, call delegate_session exactly once with workerType reviewer, kind read, context fresh. The child task is: inspect internal/supervisor/lifecycle.go and return exactly <MARKER> after the inspection. After the tool returns, reply with exactly <MARKER>.
```

Require:

- exactly one root `tool_execution_start` for `delegate_session`;
- one bound child with local provider/model metadata;
- one natural child completion and disappearance;
- the marker in the root final text; and
- a later successful root RPC turn.

- [ ] **Step 4: Implement exact three-tool parallel acceptance**

On the same still-usable root, send an exact prompt requiring three `delegate_session` calls in one parallel tool batch, each with a distinct marker and a different small repository inspection. Require:

- three `delegate_session` tool starts from the root;
- one tree snapshot with three simultaneously live children;
- three distinct session/Pi/resource identities;
- three natural results and all markers in the root aggregate response;
- no duplicate terminal child event; and
- complete child resource cleanup.

If the model emits fewer than three tool calls, preserve provider/Pi/SSE evidence and fail. Do not silently retry the prompt or fall back to direct API calls; Task 2 already provides the diagnostic comparison.

- [ ] **Step 5: Format, compile, and commit model scenarios**

Run:

```bash
gofmt -w internal/supervisor/live_rpc_lifecycle_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
go test -tags=incus ./internal/supervisor -run 'TestValidateLifecycleModelPolicy|TestLifecycleEventJournal' -count=1
git diff --check
git add internal/supervisor/live_rpc_lifecycle_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
git commit -m "test: cover local model delegated child lifecycle"
```

---

### Task 4: Add interrupt, steer, and rapid-control scenarios

**Files:**
- Modify: `internal/supervisor/live_rpc_lifecycle_test.go`
- Modify: `internal/supervisor/live_rpc_lifecycle_support_test.go` to add `waitLifecycleStreaming` and typed `get_state` decoding
- Test: `internal/supervisor/live_rpc_lifecycle_test.go`

**Interfaces:**
- Consumes: Task 1 journal and `liveAcceptance.rpc` routed through the root socket.
- Produces: `TestLiveRPCInterruptLifecycle`, `TestLiveRPCSteerLifecycle`, and `TestLiveRPCRapidControlLifecycle`.

- [ ] **Step 1: Write the three failing live test entry points**

Each entry point calls `runLifecycleScenario` and one undefined exercise method. Use exact names:

```go
func TestLiveRPCInterruptLifecycle(t *testing.T)
func TestLiveRPCSteerLifecycle(t *testing.T)
func TestLiveRPCRapidControlLifecycle(t *testing.T)
```

- [ ] **Step 2: Verify missing methods fail compilation**

Run:

```bash
go test -tags=incus ./internal/supervisor -run '^TestLiveRPC(Interrupt|Steer|RapidControl)Lifecycle$' -count=1
```

Expected: compile failure naming the missing exercise methods.

- [ ] **Step 3: Implement interrupt on root and model-created child**

For each target:

1. Start a long generation and wait for both lifecycle `running` and `get_state.data.isStreaming=true`.
2. Record the current `agent_settled` count.
3. route `{"type":"abort"}` to the target and require `success=true`.
4. Require exactly one new settlement, `isStreaming=false`, and an open RPC transport.
5. For a root, send a normal marker prompt and require completion.
6. For a child, require the blocked child call to settle with `child_aborted`, disappear, and leave the root usable.

A transport EOF, missing settlement, duplicate settlement, or noncanonical child result is a failure.

- [ ] **Step 4: Implement steer during root and child generation**

Start generation with an original prompt that cannot contain the run-unique steer marker. Wait for streaming, route:

```go
map[string]any{"type": "steer", "message": "Stop the prior response and include exactly " + marker + "."}
```

Require immediate `success=true`, one eventual settlement, the marker in final assistant text, `isStreaming=false`, and a successful later turn. Repeat against a model-created child and require its terminal result to contain the steer marker.

- [ ] **Step 5: Implement rapid steer → abort → follow-up**

On a running root, send steer and require its acknowledgement, immediately send abort and require its acknowledgement, then wait until `get_state` reports both `isStreaming=false` and `pendingMessageCount=0`. Record the settlement count, send a new prompt with a second marker, and require exactly one additional settlement. Require the second marker and require the prior steer marker not to appear in the follow-up's final assistant text.

- [ ] **Step 6: Run focused hermetic control tests, format, and commit**

Run:

```bash
gofmt -w internal/supervisor/live_rpc_lifecycle_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
go test ./internal/manager -run 'Test(Steer|Interrupt|SendMessage)' -count=1
go test ./internal/supervisor -run 'TestLocalSession|TestReadChildResult' -count=1
go test -race ./internal/manager ./internal/supervisor -run 'Test(Steer|Interrupt|SendMessage|LocalSession|ReadChildResult)' -count=1
go test -tags=incus ./internal/supervisor -run 'TestValidateLifecycleModelPolicy|TestLifecycleEventJournal' -count=1
git diff --check
git add internal/supervisor/live_rpc_lifecycle_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
git commit -m "test: cover interrupt and steer lifecycle transitions"
```

---

### Task 5: Add mixed sibling outcomes and final per-scenario invariants

**Files:**
- Modify: `internal/supervisor/live_rpc_lifecycle_test.go`
- Modify: `internal/supervisor/live_rpc_lifecycle_support_test.go`
- Test: both new files

**Interfaces:**
- Consumes: concurrent child calls, routed RPC, journal, exact cleanup, and root usability helpers.
- Produces: `TestLiveRPCMixedSiblingLifecycle` and shared final invariant checks used by all eight scenarios.

- [ ] **Step 1: Write the mixed-outcome test before implementation**

```go
func TestLiveRPCMixedSiblingLifecycle(t *testing.T) {
    runLifecycleScenario(t, "mixed-siblings", func(h *liveAcceptance) {
        h.exerciseMixedSiblingOutcomes()
    })
}
```

- [ ] **Step 2: Verify the missing implementation fails compilation**

Run:

```bash
go test -tags=incus ./internal/supervisor -run '^TestLiveRPCMixedSiblingLifecycle$' -count=1
```

Expected: FAIL because `exerciseMixedSiblingOutcomes` is undefined.

- [ ] **Step 3: Implement three simultaneous sibling outcomes**

Start three child calls concurrently with distinct markers and enough work to observe all three. After one tree snapshot contains all children:

- allow child A to complete naturally and require HTTP 200 with marker A;
- stop child B through DELETE and require its pending call to settle with the same canonical stop code established in Task 2;
- abort child C through routed RPC and require `child_aborted`;
- require A's result and events to remain unchanged by B/C controls;
- require all children to disappear independently; and
- require a later root prompt to complete.

Track child IDs before applying controls; never identify targets by mutable slice position after the tree changes.

- [ ] **Step 4: Add shared terminal invariants and safe artifacts**

Before each scenario marks success, write:

- ordered `lifecycle-events.json`;
- normalized `lifecycle-actions.json` without request bodies, URLs, credentials, or raw errors;
- pre-control, post-control, and final tree snapshots; and
- exact resource snapshots.

Assert monotonic broker sequence, monotonic per-session source sequence, no duplicate `(sessionID, sourceSeq)`, one terminal settlement per started generation, no remaining descendant socket, no owned process, and no proxy warning.

- [ ] **Step 5: Format, compile, race support tests, and commit**

Run:

```bash
gofmt -w internal/supervisor/live_rpc_lifecycle_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
go test -tags=incus ./internal/supervisor -run 'TestValidateLifecycleModelPolicy|TestLifecycleEventJournal' -count=1
go test -race -tags=incus ./internal/supervisor -run 'TestValidateLifecycleModelPolicy|TestLifecycleEventJournal' -count=1
git diff --check
git add internal/supervisor/live_rpc_lifecycle_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
git commit -m "test: cover mixed parallel child outcomes"
```

---

### Task 6: Execute the live matrix and convert each failure into a root-cause regression

**Files:**
- Read: the failed run directory printed under `~/.cache/kanedias/e2e/`
- Read: `internal/supervisor/node.go`, `local.go`, `children.go`, `router.go`, `result.go`, `pirpc/client.go`, `internal/supervisorapi/client.go`, `internal/manager/pi.go`, and `internal/proxy/observability.go` only as indicated by the first divergent boundary
- Modify/Test: add exact paths to this plan after reproduction and before making a production edit

**Interfaces:**
- Consumes: all eight live tests.
- Produces: a clean one-pass matrix, plus one narrow regression and minimal fix per confirmed defect.

- [ ] **Step 1: Verify a clean host baseline and local provider readiness**

Run from the worktree:

```bash
set -a
. /home/steven/source/github/kanedias/.env
set +a
incus list 'kanedias_session-*' --format csv -c ns4
incus storage volume list default --format csv | grep 'kanedias_workspace-' || true
ss -ltnp | grep -E ':(13305|9010)\b'
```

Expected: no test-owned session instances or workspace clones; the configured local provider endpoint is listening. Do not start a second provider if one is already healthy.

- [ ] **Step 2: Run the existing isolated lifecycle acceptances first**

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLive(ChildLivenessShutdown|ServerManagedSupervisor)Acceptance$' \
  -timeout 90m
```

Expected: PASS and exact baseline restoration. A failure here takes priority because it predates the new matrix.

- [ ] **Step 3: Run every new scenario once**

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveRPC.*Lifecycle$' \
  -timeout 4h
```

Expected: all eight tests PASS. Preserve the printed artifact path for any failure.

- [ ] **Step 4: On failure, identify the first divergent boundary**

Use the retained provider/Pi/supervisor/proxy logs, `lifecycle-events.json`, action records, tree snapshots, process state, and Incus snapshots. Classify only after locating the earliest divergence:

1. provider response or missing `delegate_session` tool call;
2. Pi RPC command/event/settlement;
3. supervisor route/state/terminal result;
4. descendant SSE/mirror propagation;
5. proxy teardown; or
6. process/socket/Incus cleanup.

Load and follow the systematic-debugging skill. Do not edit code during evidence collection.

- [ ] **Step 5: Amend this plan with the exact regression and fix**

For each confirmed defect, append a numbered task before Task 7 containing:

- the exact failing invariant and artifact evidence;
- exact production and test file paths;
- the failing unit/integration test code;
- the command proving RED;
- the minimal implementation change;
- focused normal and race commands proving GREEN; and
- a dedicated commit message.

Then execute that task with TDD, rerun the failed live scenario once, and restart its five-run count. This evidence-gated amendment avoids guessing production changes before a defect is reproduced.

- [ ] **Step 6: Repeat until the one-pass matrix is clean**

After each fix, rerun focused tests, the failed scenario, then the complete one-pass matrix. Continue until one complete pass leaves no test-owned Incus instance, volume, process, socket, or proxy warning.

---

### Task 7: Prove five consecutive clean runs and complete verification

**Files:**
- Verify: all changed files
- Update: `docs/superpowers/specs/2026-08-10-rpc-lifecycle-hardening-design.md` only if implementation evidence required a design clarification

**Interfaces:**
- Consumes: the final live suite and every regression added in Task 6.
- Produces: final stability and repository verification evidence.

- [ ] **Step 1: Run the complete lifecycle matrix five independent times**

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=5 -tags=incus ./internal/supervisor \
  -run '^TestLiveRPC.*Lifecycle$' \
  -timeout 12h
```

Expected: every scenario passes five times after the final code change. Any failure resets that scenario to zero and returns to Task 6.

- [ ] **Step 2: Rerun existing live lifecycle acceptance**

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLive(RecursiveSupervisor|ChildLivenessShutdown|ServerManagedSupervisor)Acceptance$' \
  -timeout 3h
```

Expected: PASS with exact resource cleanup.

- [ ] **Step 3: Run full hermetic, race, build, lint, and diff gates**

```bash
make test
go test -race ./internal/supervisor/... ./internal/supervisorapi ./internal/manager ./internal/proxy
make build
make lint
git diff --check
```

Expected: every command exits zero and lint reports zero issues.

- [ ] **Step 4: Verify no live resource or warning residue**

```bash
incus list 'kanedias_session-*' --format csv -c ns4
incus storage volume list default --format csv | grep 'kanedias_workspace-' || true
find "${XDG_RUNTIME_DIR:-/tmp}" -maxdepth 3 -type s -name '*kanedias*.sock' -print 2>/dev/null || true
ps -eo pid,ppid,stat,args | grep -E '[k]anedias-under-test|[k]anedias session'
```

Expected: no test-owned resource, socket, or process. Inspect final retained logs, if any, for `proxy internal warning`, bounded-capacity failures, replay gaps, raw URLs, or credentials.

- [ ] **Step 5: Review the final diff and commit any verification-only documentation**

```bash
git status --short --branch
git diff origin/main...HEAD --stat
git diff origin/main...HEAD --check
git log --oneline origin/main..HEAD
```

If the design needed no clarification, do not create an empty documentation commit. Record exact commands and outcomes in the task handoff, including residual risks such as local-provider availability and the opt-in nature of Incus tests.
