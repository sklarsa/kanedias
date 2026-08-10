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

### Task 6A: Bound stalled supervisor SSE writes so root shutdown stays clean

**Failed invariant and evidence:**

- Required invariant: after an accepted root `DELETE`, a successfully settled root and child must exit zero, unlink the supervisor socket, and restore the exact Incus baseline even when a connected SSE client does not read.
- The required Task 6 Step 2 command failed first at `internal/supervisor/live_incus_test.go:625` with `liveness root exited after DELETE: exit status 1`; `/home/steven/.cache/kanedias/e2e/e2e-3244740-1786390399344309628/liveness-root.log` ends with `Error: context deadline exceeded`.
- The focused reproduction failed identically at `/home/steven/.cache/kanedias/e2e/e2e-3253771-1786390774846879783/`. Its `liveness-events.sse` and `fresh-read-complete-tree.json` prove successful child output, root settlement, and child disappearance before DELETE. Its `incus-operation-monitor.log` proves root stop/delete completed by `2026-08-10T15:40:45.906434293-04:00`, while `liveness-root.log` did not finish until `2026-08-10 15:41:10.831269249 -0400` with the 30-second deadline error; `teardown-final-incus.json` matches baseline.
- Earliest divergent boundary: `internal/supervisorapi/events.go:33-43` can block indefinitely in synchronous SSE write/flush to the deliberately non-reading raw Unix client retained by `internal/supervisor/live_incus_test.go:616-625`. Broker closure cannot release a handler already blocked in the response write, so `internal/supervisorapi/unix.go:121-135` reaches its shutdown deadline and downgrades otherwise successful cleanup to process exit 1.

**Files:**

- Modify/Test: `internal/supervisorapi/handler_test.go`
- Modify: `internal/supervisorapi/events.go`
- Live verification only: `internal/supervisor/live_incus_test.go`
- Read artifacts: `/home/steven/.cache/kanedias/e2e/e2e-3244740-1786390399344309628/` and `/home/steven/.cache/kanedias/e2e/e2e-3253771-1786390774846879783/`

**Interfaces:**

- Consumes: `supervisor.NewEventBroker`, `StartUnix`, `NewHandler`, `http.NewResponseController`, and the existing consuming-stream shutdown test.
- Produces: bounded supervisor SSE response writes that disconnect a non-reading client without changing event retention or subscriber mailbox limits.

- [ ] **Step 1: Add the raw non-reading Unix SSE regression**

In `internal/supervisorapi/handler_test.go`, add a `subscribeCalled chan struct{}` field to `fakeService`. In `fakeService.Subscribe`, close it when non-nil immediately before returning `service.sub`. Then add this test beside `TestActiveParentToChildSSEBrokerCloseAllowsPromptCleanUnixShutdown`:

```go
func TestStalledSupervisorSSEClientDoesNotBlockUnixShutdown(t *testing.T) {
    path := filepath.Join(t.TempDir(), "stalled-supervisor.sock")
    broker := supervisor.NewEventBroker()
    defer broker.Close()

    payload := json.RawMessage(`{"chunk":"` + strings.Repeat("x", 96<<10) + `"}`)
    for range 96 {
        broker.PublishLocal("root", "pi", payload)
    }
    subscribed := make(chan struct{})
    service := &fakeService{sub: broker.Subscribe(), subscribeCalled: subscribed}
    server, err := StartUnix(path, NewHandler(service))
    if err != nil {
        t.Fatal(err)
    }

    stalled, err := net.Dial("unix", path)
    if err != nil {
        t.Fatal(err)
    }
    defer stalled.Close()
    if _, err := io.WriteString(stalled, "GET /v1/events HTTP/1.1\r\nHost: unix\r\nAccept: text/event-stream\r\n\r\n"); err != nil {
        t.Fatal(err)
    }
    select {
    case <-subscribed:
    case <-time.After(time.Second):
        t.Fatal("stalled SSE request did not subscribe")
    }

    broker.Close()
    shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel()
    started := time.Now()
    if err := server.Close(shutdownCtx); err != nil {
        t.Fatalf("stalled SSE shutdown error = %v", err)
    }
    if elapsed := time.Since(started); elapsed >= 2*time.Second {
        t.Fatalf("stalled SSE shutdown took %s, want under 2s", elapsed)
    }
    if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
        t.Fatalf("Unix socket remains after stalled SSE shutdown: %v", err)
    }
}
```

The 96 events contain approximately 9 MiB of valid bounded JSON: enough to fill a Unix socket whose peer never reads, while remaining below the existing 4,096-event and 16-MiB broker limits. Do not add sleeps; the `subscribeCalled` seam is the observable request-admission boundary.

- [ ] **Step 2: Run the focused regression and prove RED**

Run:

```bash
go test -v -count=1 ./internal/supervisorapi \
  -run '^TestStalledSupervisorSSEClientDoesNotBlockUnixShutdown$'
```

Expected: FAIL after the three-second shutdown context expires. The failure must contain:

```text
stalled SSE shutdown error = context deadline exceeded
```

This proves the raw non-reading client, not broker admission, Pi settlement, or Incus cleanup, pins the active HTTP handler.

- [ ] **Step 3: Bound each supervisor SSE response write and flush**

Modify only `internal/supervisorapi/events.go` in production:

1. Add the named private constant `const supervisorSSEWriteTimeout = time.Second`.
2. Construct one `controller := http.NewResponseController(w)` after preserving the existing streaming-support check.
3. Add a private helper that sets `controller.SetWriteDeadline(time.Now().Add(supervisorSSEWriteTimeout))` immediately before each response operation, executes one header/event write and `controller.Flush()`, and clears the deadline with `controller.SetWriteDeadline(time.Time{})` on every return path after a successful deadline set. Join a clear failure to the operation error rather than hiding either error. Treat `http.ErrNotSupported` from setting a deadline as an unsupported optional capability so existing recorder-based handler tests continue through the existing `http.Flusher` seam; real Unix HTTP connections must use the deadline.
4. Use that helper for the initial `WriteHeader(http.StatusOK)` plus flush and for every complete SSE frame `fmt.Fprintf` plus flush. Return from `serveEvents` on any write, flush, deadline-set, or deadline-clear error.
5. Reset to a fresh absolute deadline for every frame; never carry one frame's deadline into the wait for the next broker event, and never leave a nonzero deadline on the connection after a successful flush.
6. Do not change `supervisor.EventBroker`, `eventmailbox`, `DefaultEventRingCapacity`, `DefaultEventRingByteCapacity`, configured `max_events`/`max_bytes`, replay behavior, or subscriber mailbox capacity. Normal consuming streams retain the same ordering and payloads.

- [ ] **Step 4: Prove focused GREEN under normal and race execution**

Run:

```bash
gofmt -w internal/supervisorapi/events.go internal/supervisorapi/handler_test.go
go test -v -count=1 ./internal/supervisorapi \
  -run '^Test(StalledSupervisorSSEClientDoesNotBlockUnixShutdown|ActiveParentToChildSSEBrokerCloseAllowsPromptCleanUnixShutdown)$'
go test -race -v -count=1 ./internal/supervisorapi \
  -run '^Test(StalledSupervisorSSEClientDoesNotBlockUnixShutdown|ActiveParentToChildSSEBrokerCloseAllowsPromptCleanUnixShutdown)$'
go test -count=1 ./internal/supervisorapi
git diff --check
```

Expected: both the stalled-client regression and existing consuming-client comparison PASS normally and with the race detector; the complete `internal/supervisorapi` package passes; `git diff --check` is silent.

- [ ] **Step 5: Rerun the failed live acceptance once**

Run only the confirmed failed scenario first:

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveChildLivenessShutdownAcceptance$' \
  -timeout 90m
```

Expected: PASS with zero process exit status, root and child sockets absent, no test-owned process, exact Incus baseline restoration, and no proxy warning. Preserve a new artifact path if it fails; do not broaden the fix.

- [ ] **Step 6: Commit the bounded SSE fix**

```bash
git add internal/supervisorapi/events.go internal/supervisorapi/handler_test.go
git commit -m "fix: bound stalled supervisor SSE writes"
```

- [ ] **Step 7: Resume Task 6 in failure priority order**

After GREEN, restart Task 6 at Step 2. Run both existing isolated acceptances once. Diagnose the separate `TestLiveServerManagedSupervisorAcceptance` missing-bootstrap failure next with its retained `/home/steven/.cache/kanedias/e2e/e2e-3244740-1786390502809184281/managed-server.log` evidence and another plan amendment before any additional production edit. Only after both existing acceptances pass may Task 6 Step 3 run the eight new `TestLiveRPC.*Lifecycle` scenarios.

---

### Task 6B: Honor the configured managed-server authentication mode in live acceptance

**Failed invariant and evidence:**

- Required invariant: `TestLiveServerManagedSupervisorAcceptance` must connect to a successfully started managed server using the authentication mode selected by its generated configuration, exercise the initial and restarted server, and restore the exact Incus baseline.
- Task 6 Step 2 failed at `internal/supervisor/live_incus_test.go:1652` after 122.58 seconds while waiting for `bootstrap URL and effective address in log`; retained artifacts are at `/home/steven/.cache/kanedias/e2e/e2e-3304814-1786392737132818170/`.
- `/home/steven/.cache/kanedias/e2e/e2e-3304814-1786392737132818170/managed-config.toml` resolves `[server] require_session = false`. Its `managed-server.log` proves successful product startup with `effective_address=127.0.0.1:45403` and `Web UI: http://steven-desktop:45403/`, correctly contains no `Bootstrap URL:`, and records clean shutdown. `baseline-incus.json` and `teardown-final-incus.json` match.
- Earliest divergent boundary: `waitForBootstrapURL` and `bootstrapManagedServer` in `internal/supervisor/live_incus_test.go` unconditionally require and exchange a bootstrap capability even though trusted mode intentionally emits no capability. This is acceptance-contract drift introduced when session authentication became opt-in, not a production startup defect.

**Files and scope:**

- Modify/Test only: `internal/supervisor/live_incus_test.go`
- Read comparison: `internal/server/handler_test.go`
- Read artifacts: `/home/steven/.cache/kanedias/e2e/e2e-3304814-1786392737132818170/`
- Do not modify any production file. Do not change configuration defaults, server output, authentication middleware, manager behavior, or live lifecycle semantics.

**Interfaces:**

- Consumes: `config.ServerConfig.Resolve`, `http.Client`, `cookiejar.New`, the server's `effective_address=`, `Web UI:`, and `Bootstrap URL:` output contracts.
- Produces: a mode-aware managed live-acceptance connection that uses the effective loopback listener for host-side requests and performs bootstrap only when the resolved configuration requires it.

Independent review of this Task 6B amendment is required before Step 1 edits `internal/supervisor/live_incus_test.go`.

- [ ] **Step 1: Add RED parser, origin-validation, and hermetic HTTP authority regressions**

In `internal/supervisor/live_incus_test.go`, extract a pure parser whose state keeps transport identity separate from browser identity:

```go
type managedServerStartup struct {
    effectiveOrigin  string
    browserOrigin    string
    browserAuthority string
    bootstrapURL     string
}

func parseManagedServerStartup(text string, requireSession bool, configuredHostname string) (managedServerStartup, bool)
```

Add `TestParseManagedServerStartupModes` with these exact table cases:

1. `trusted-ready-configured-hostname`: input contains `effective_address=127.0.0.1:45403` and `Web UI: http://steven-desktop:45403/` but no bootstrap line; with `requireSession=false` and configured hostname `steven-desktop`, it is ready, effective origin is `http://127.0.0.1:45403`, browser authority is `steven-desktop:45403`, browser origin is `http://steven-desktop:45403`, and bootstrap URL is empty.
2. `trusted-ready-empty-hostname`: the same effective address and `Web UI: http://127.0.0.1:45403/`; with an empty configured hostname, it is ready and browser authority falls back to `127.0.0.1:45403`.
3. `trusted-missing-web-ui`: input contains only the effective address; with `requireSession=false`, it is not ready.
4. `authenticated-ready`: input contains `effective_address=127.0.0.1:45403` and `Bootstrap URL: http://steven-desktop:45403/bootstrap?capability=test-token`; with `requireSession=true` and configured hostname `steven-desktop`, it is ready and returns the effective loopback origin, canonical advertised browser origin/authority, and complete advertised bootstrap URL.
5. `authenticated-missing-bootstrap`: input contains the effective address and Web UI only; with `requireSession=true`, it is not ready.
6. `malformed-effective-address` and `non-loopback-effective-address`: `effective_address=not-an-authority` and `effective_address=192.0.2.10:45403` are not ready in either authentication mode.

Add `TestNormalizeManagedBootstrapURLToEffectiveOrigin`. Given effective origin `http://127.0.0.1:45403` and advertised URL `http://steven-desktop:45403/bootstrap?capability=test-token`, require exactly `http://127.0.0.1:45403/bootstrap?capability=test-token`. Include a relative `/bootstrap?capability=test-token` case with the same result. Add rejection cases for HTTPS, non-loopback host, userinfo, nonempty path (including `/`), query, and fragment on the effective origin. These cases prove the dial target is a plain HTTP loopback origin and that only the advertised bootstrap path/query is transplanted.

Add two hermetic `httptest` regressions using a listener at an effective `127.0.0.1:PORT` URL and a distinct configured hostname `steven-desktop`:

1. `TestManagedServerConnectionAuthenticatedAuthority` supplies an advertised bootstrap URL `http://steven-desktop:PORT/bootstrap?capability=test-token`. Its server records that the bootstrap request reached the effective listener with exact path `/bootstrap` and query `capability=test-token`, received `Host: steven-desktop:PORT`, returned a 303 and a session cookie, and then received both `GET /` and subsequent `postNewSession` and `postDatastar` writes through the same effective listener. Require the cookie on those later requests and require their `Host` to equal `steven-desktop:PORT` and `Origin` to equal `http://steven-desktop:PORT`.
2. `TestManagedServerConnectionTrustedNoBootstrap` supplies trusted startup state, counts bootstrap requests, and requires zero. It still requires `GET / == 200`; a subsequent write dials the effective listener and may use the effective authority for `Host` and `Origin` because browser security is disabled.

The hermetic tests must not edit `/etc/hosts`, resolve `steven-desktop`, source `.env`, or require Incus. The advertised hostname is header identity only; every request URL and cookie-jar key remains the effective loopback URL.

- [ ] **Step 2: Run the focused tests and prove RED**

Run:

```bash
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(ParseManagedServerStartupModes|NormalizeManagedBootstrapURLToEffectiveOrigin|ManagedServerConnection(AuthenticatedAuthority|TrustedNoBootstrap))$'
```

Expected: FAIL to compile because the new parser, normalization, connection-state, and request-authority seams do not exist yet. The failure must be limited to those missing test-only seams; do not edit production to obtain RED.

- [ ] **Step 3: Parse and validate the effective listener while deriving canonical browser identity**

Modify only `internal/supervisor/live_incus_test.go`:

1. Implement `parseManagedServerStartup` without filesystem, process, network, test-harness, or timing dependencies. Extract the logged `effective_address`, parse it with `net.SplitHostPort`, require a non-nil `net.ParseIP(host)` whose `IsLoopback()` is true, require a nonempty port, and reconstruct the authority with `net.JoinHostPort`. A malformed or non-loopback address is not ready. Set `effectiveOrigin` to `http://` plus that reconstructed authority; it remains the URL and dial target for every host-side request.
2. Derive `browserAuthority` exactly like production `advertisedAddress`: use `configuredHostname` when nonempty, otherwise the effective host, and combine it with the effective listener port using `net.JoinHostPort`. Derive `browserOrigin` as `http://` plus this authority. Do not derive browser identity from the request URL or DNS, and do not use the advertised hostname as a dial target.
3. When `requireSession=false`, readiness requires the validated effective address and a nonempty `Web UI:` line and returns no bootstrap URL. When `requireSession=true`, readiness requires the validated effective address and a nonempty `Bootstrap URL:` line; Web UI output is not an authentication prerequisite.
4. Replace `waitForBootstrapURL` with `waitForManagedServerStartup(logPath string, requireSession bool, configuredHostname string) managedServerStartup`. Keep the existing two-minute `h.poll`, read the current log each poll, and delegate all output interpretation, authority derivation, and readiness decisions to the pure parser.
5. Implement `normalizeManagedBootstrapURL(effectiveOrigin, advertised string) (string, error)` using `net/url` and `net.SplitHostPort`. Require the effective URL to have scheme exactly `http`, no userinfo, no opaque component, no path or raw path, no query/forced query, and no fragment; require its host to be a syntactically valid loopback IP authority with a nonempty port. Resolve a relative advertised bootstrap reference against that origin. For an absolute advertised bootstrap URL, replace only its scheme and host with the validated effective scheme and authority, preserving escaped path, raw query, and fragment. Never request the advertised hostname.

- [ ] **Step 4: Carry effective and browser origins through connection and every managed write**

Add a test-only `managedServerConnection` value that carries the `*http.Client`, `requireSession`, `effectiveOrigin`, `browserOrigin`, and `browserAuthority`. Rename `bootstrapManagedServer` to `connectManagedServer`; pass `requireSession` and the resolved configured hostname. Preserve log selection for `managed-server.log` and `managed-server-restart.log`, then:

1. Call `waitForManagedServerStartup(logPath, requireSession, configuredHostname)` and retain all returned transport/browser state.
2. Always construct an `http.Client` with a new cookie jar, the existing 30-second timeout, and the existing no-follow `CheckRedirect`. The jar remains keyed to each effective loopback request URL; do not rewrite cookies to the advertised hostname.
3. Only when `requireSession=true`, normalize the advertised bootstrap URL onto `effectiveOrigin`, build an explicit GET request to that effective URL, set `Request.Host = browserAuthority`, issue it through the jar client, close its body, and require HTTP 303 See Other. When `requireSession=false`, issue no bootstrap request.
4. In both modes, build an explicit GET request to `effectiveOrigin + "/"`. In authenticated mode set `Request.Host = browserAuthority`; in trusted mode retain the effective authority. Issue it through the same client so the authenticated cookie associated with the effective URL is sent, close its body, and require HTTP 200 OK.
5. Return `managedServerConnection` only after readiness/authentication succeeds. Add `managedRequestIdentity { authority, origin string }` and `func (c managedServerConnection) requestIdentity() managedRequestIdentity`; authenticated mode returns `browserAuthority`/`browserOrigin`, while trusted mode parses and returns the effective URL authority/origin.

Change the helper contracts to `postNewSession(client *http.Client, fullURL string, identity managedRequestIdentity, body any)` and `func (h *liveAcceptance) postDatastar(client *http.Client, fullURL string, identity managedRequestIdentity, body any)`. They must always send `fullURL` under `effectiveOrigin` so transport dials loopback, while setting `Request.Host = identity.authority` and `Origin = identity.origin`. Keep `Sec-Fetch-Site: same-origin`, content type, response validation, and all existing payload behavior unchanged. Update every managed call site:

- both initial `postNewSession` calls use the initial connection's effective origin and identity;
- initial root steer and every initial `answerManagedQuestion` Datastar write use the initial connection identity;
- after restart, descendant steer, interrupt, question answer, descendant stop, and both root stops use the restarted connection's effective origin and identity;
- change `answerManagedQuestion` to accept the connection/request identity rather than reconstructing Host and Origin from `serverOrigin`.

Existing standalone `postNewSession` contract tests must pass an explicit effective request identity so their behavior remains unchanged. No JSON or Datastar write may reconstruct browser identity from an effective request URL.

In `runServerManaged`, immediately after `config.Load`, call `managedCfg.Server.Resolve()` once, fail with `resolve managed server configuration` on error, and retain both `resolvedServer.RequireSession` and `resolvedServer.Hostname`. Pass those same resolved values to both initial and restarted `connectManagedServer` calls. Use each returned connection's effective origin for all URLs and reads, while using its selected request identity for all writes. Do not infer mode or authority from whichever log lines happen to appear.

- [ ] **Step 5: Prove focused GREEN, race safety, tagged compilation, and the production output comparison**

Run exactly:

```bash
gofmt -w internal/supervisor/live_incus_test.go
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(ParseManagedServerStartupModes|NormalizeManagedBootstrapURLToEffectiveOrigin|ManagedServerConnection(AuthenticatedAuthority|TrustedNoBootstrap))$'
go test -race -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(ParseManagedServerStartupModes|NormalizeManagedBootstrapURLToEffectiveOrigin|ManagedServerConnection(AuthenticatedAuthority|TrustedNoBootstrap))$'
go test -count=1 -tags=incus ./internal/supervisor -run '^$'
go test -v -count=1 ./internal/server \
  -run '^TestHandler(PrintsAdvertisedURLs|TrustedNetworkModeBypassesBrowserSecurity)$'
git diff --check
```

Expected: parser/normalization and both hermetic connection tests pass normally and under the race detector; authenticated mode proves effective-loopback dialing, advertised bootstrap path/query, cookie retention on the effective URL, advertised Host/Origin on JSON and Datastar writes, and successful responses; trusted mode proves no bootstrap request and successful effective-authority access. The tagged supervisor package compiles; existing server tests continue proving production output/mode behavior; `git diff --check` is silent.

- [ ] **Step 6: Rerun only the failed managed live acceptance**

Run:

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveServerManagedSupervisorAcceptance$' \
  -timeout 90m
```

Expected: PASS through initial trusted connection, two managed root launches, server restart, trusted reconnection without bootstrap, fleet rediscovery, descendant controls, root cleanup, process/socket cleanup, and exact Incus baseline restoration. If it fails, preserve the new artifact directory and return to read-only systematic diagnosis; do not broaden this test-only fix.

- [ ] **Step 7: Commit the acceptance-mode fix**

After focused and live GREEN:

```bash
git add internal/supervisor/live_incus_test.go
git commit -m "test: honor managed server authentication mode"
```

- [ ] **Step 8: Resume Task 6 only after live GREEN**

First rerun Task 6 Step 2 exactly:

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLive(ChildLivenessShutdown|ServerManagedSupervisor)Acceptance$' \
  -timeout 90m
```

Only after both existing acceptances pass and restore the exact baseline, run Task 6 Step 3 exactly:

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveRPC.*Lifecycle$' \
  -timeout 4h
```

Do not count or run any Step 3 scenario before the managed live acceptance is GREEN.

---

### Task 6C: Evaluate managed fleet rediscovery per SSE patch instead of across the whole stream

**Failed invariant and evidence:**

- Required invariant: after a non-destructive managed-server restart, `TestLiveServerManagedSupervisorAcceptance` must accept a fleet snapshot containing each surviving managed root exactly once, even when the manager subsequently publishes another valid replacement snapshot.
- The Task 6B live rerun failed at `internal/supervisor/live_incus_test.go:1907` after 42.31 seconds while waiting for `restarted fleet contains all managed roots`; retained artifacts are at `/home/steven/.cache/kanedias/e2e/e2e-3335325-1786394270012822146/`.
- Both pre-restart socket snapshots in that artifact show distinct expected session IDs with lifecycle `running`. `managed-server-restart.log` proves trusted reconnection succeeded and six `GET /ui/fleet` requests returned HTTP 200. The Incus timeout snapshot shows both root instances and volumes still running, and teardown exactly restored the baseline.
- Earliest divergent boundary: `assertFleetContainsExactly` in `internal/supervisor/live_incus_test.go` scans each five-second SSE response to EOF, concatenates every event, then requires each expected `data-root` marker to occur exactly once across that whole response. `internal/manager/monitor.go` publishes a fleet revision after each configured one-second root snapshot, and `internal/server/handler.go` renders each revision as another complete `fleet-panel` replacement patch. Therefore a healthy stream containing two valid snapshots has two copies of each marker and the acceptance helper deterministically rejects it. This is a latent test-helper defect, not failed manager rediscovery or production SSE behavior.

**Files and scope:**

- Modify/Test only: `internal/supervisor/live_incus_test.go`
- Read comparison only: `internal/manager/monitor.go`, `internal/server/handler.go`, and `internal/server/handler_test.go`
- Read artifacts: `/home/steven/.cache/kanedias/e2e/e2e-3335325-1786394270012822146/`
- Do not modify manager revision publication, fleet rendering, SSE framing, discovery timing, production authentication, or any production file.

**Interfaces:**

- Consumes: the Datastar `event: datastar-patch-elements` fleet stream and the stable top-level `data-root="SESSION_ID"` marker emitted once per root by `internal/server/web/fleet.html`.
- Produces: a pure, bounded scanner that evaluates one complete SSE patch at a time and returns as soon as one patch contains exactly the expected root set.

Independent review of this Task 6C amendment is required before Step 1 edits `internal/supervisor/live_incus_test.go`.

- [ ] **Step 1: Add a hermetic repeated-snapshot regression**

In `internal/supervisor/live_incus_test.go`, add a test-only helper:

```go
func fleetStreamContainsExactly(r io.Reader, roots []managedRoot) (bool, error)
```

Add `TestFleetStreamContainsExactlyEvaluatesIndividualPatches` beside the other hermetic managed-server helper tests. Build two complete `event: datastar-patch-elements` events separated by blank lines, with each event containing exactly one `data-root` marker for each of two expected roots. Require `fleetStreamContainsExactly` to return true even though the full input contains each marker twice. Add comparison cases requiring false for a single patch that is missing an expected root, duplicates one expected marker within that patch, or contains an unexpected root. The test must use only in-memory strings and must not source `.env`, start a server, or require Incus despite the file's existing `incus` build tag.

- [ ] **Step 2: Prove RED against whole-stream counting**

Run:

```bash
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestFleetStreamContainsExactlyEvaluatesIndividualPatches$'
```

Expected: FAIL to compile because `fleetStreamContainsExactly` does not exist. The RED must be limited to that missing test-only helper.

- [ ] **Step 3: Scan and validate complete individual SSE patches**

Modify only `internal/supervisor/live_incus_test.go`:

1. Implement `fleetStreamContainsExactly` with `bufio.Scanner`, accumulating one SSE event until its blank-line boundary. Set an explicit bounded one-MiB maximum token size so a legitimate fleet line is not constrained by Scanner's 64-KiB default while oversized input still fails closed. Evaluate only complete `event: datastar-patch-elements` events.
2. A patch matches only when the total number of `data-root="` markers equals `len(roots)` and every expected `data-root="SESSION_ID"` marker occurs exactly once in that same patch. This rejects missing, duplicated, and unexpected roots without comparing across later replacement patches.
3. Return immediately on the first matching complete patch so the acceptance does not wait for the five-second request context after rediscovery is already visible. On EOF, evaluate a final nonempty event for robustness, and return `scanner.Err()` rather than hiding malformed/oversized input failures.
4. Change `assertFleetContainsExactly` to pass `resp.Body` to the helper, close the body, and accept only `matched && err == nil`. Retain the existing five-second per-request context and 30-second outer observable-state poll. Do not add sleeps or retries and do not retain/log response bodies.

- [ ] **Step 4: Prove focused GREEN and race safety**

Run:

```bash
gofmt -w internal/supervisor/live_incus_test.go
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(FleetStreamContainsExactlyEvaluatesIndividualPatches|ParseManagedServerStartupModes|NormalizeManagedBootstrapURLToEffectiveOrigin|ManagedServerConnection(AuthenticatedAuthority|TrustedNoBootstrap))$'
go test -race -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(FleetStreamContainsExactlyEvaluatesIndividualPatches|ParseManagedServerStartupModes|NormalizeManagedBootstrapURLToEffectiveOrigin|ManagedServerConnection(AuthenticatedAuthority|TrustedNoBootstrap))$'
go test -count=1 -tags=incus ./internal/supervisor -run '^$'
git diff --check
```

Expected: the repeated valid stream is accepted; malformed individual snapshots are rejected; all Task 6B tests still pass normally and under the race detector; tagged compilation succeeds; `git diff --check` is silent.

- [ ] **Step 5: Rerun the failed managed live acceptance**

Run:

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveServerManagedSupervisorAcceptance$' \
  -timeout 90m
```

Expected: PASS through fleet rediscovery and all remaining descendant/root controls, with exact process, socket, Incus instance, and volume baseline restoration. Preserve a new artifact and resume read-only diagnosis if any later boundary fails.

- [ ] **Step 6: Commit the combined managed-acceptance hardening only after live GREEN**

Task 6B and 6C both modify the same test-only file, and Task 6B correctly remained uncommitted when its live rerun exposed this latent helper defect. After focused and live GREEN, commit the jointly verified file without manufacturing an intermediate commit that still fails the required live acceptance:

```bash
git add internal/supervisor/live_incus_test.go
git commit -m "test: harden managed server acceptance"
```

Then update both Task 6B and Task 6C reports with the shared commit and verification evidence, obtain independent implementation review, and resume Task 6 Step 2. Do not run the eight new lifecycle scenarios before both existing acceptances pass together.

---

### Task 6D: Wait for a running descendant to enter the restarted manager projection before server control

**Failed invariant and evidence:**

- Required invariant: after the restarted server rediscovers the managed roots, descendant controls through that server must begin only after the descendant is both directly running under its root supervisor and atomically present in the restarted manager's fleet tree/routes.
- The Task 6C live rerun advanced through trusted restart and fleet rediscovery, then failed at `internal/supervisor/live_incus_test.go:2876` after 11.55 seconds when the first descendant steer returned the normalized command-failure patch. Retained artifacts are at `/home/steven/.cache/kanedias/e2e/e2e-3356673-1786395184455477592/`.
- `managed-server-restart.log` proves `GET /ui/fleet` returned HTTP 200 in 322 microseconds at `16:53:14.133`, so Task 6C's per-patch rediscovery assertion succeeded. The test then logged direct root-socket discovery of descendant `session-8e338e2d50ea5632aa1b4321a1cf4dbc`, but the server steer at `16:53:14.234` failed with `session ... not found`, only 101 milliseconds after the initial fleet response.
- Earliest divergent boundary: `spawnManagedChild` gives the local model a trivial exact-reply task, `waitForManagedDescendant` accepts the first child with any nonempty session ID regardless of binding/lifecycle, and the test immediately addresses it through the restarted manager. The restarted manager learned the root tree before this direct child spawn; `internal/manager/monitor.go` installs descendant routes atomically with its next configured one-second snapshot commit. Direct root visibility therefore does not yet imply manager route visibility, and a trivial local-model task may also settle before that snapshot. The later asynchronous `managed-descendant-managed-child-result.json` 502 was captured while failure teardown stopped the session and does not establish an earlier child startup/model failure; it is not the basis for this amendment.

**Files and scope:**

- Modify/Test only: `internal/supervisor/live_incus_test.go`
- Reuse test helper only: `lifecycleActiveReadTask` from `internal/supervisor/live_rpc_lifecycle_test.go`
- Read comparison only: `internal/manager/monitor.go`, `internal/manager/discovery.go`, `internal/server/handler.go`, and `internal/server/web/fleet.html`
- Read artifacts: `/home/steven/.cache/kanedias/e2e/e2e-3356673-1786395184455477592/`
- Do not modify manager snapshot timing, route admission, server actions, supervisor child lifecycle, local provider behavior, or any production file.

**Interfaces:**

- Consumes: `isControllableChildSnapshot`, `lifecycleActiveReadTask`, the root `/v1/tree` snapshot, and complete Datastar fleet replacement patches containing one `data-session-id="SESSION_ID"` marker per projected session row.
- Produces: a directly running bound descendant selection and an observable restarted-manager projection barrier before the first descendant server action.

Independent review of this Task 6D amendment is required before Step 1 edits `internal/supervisor/live_incus_test.go`.

- [ ] **Step 1: Add hermetic running-descendant and fleet-projection regressions**

Add `TestManagedDescendantFromTreeRequiresOneRunningBoundReadChild`. Construct a root tree with exactly one child and require a new pure helper:

```go
func managedDescendantFromTree(tree supervisor.NodeSnapshot) (supervisor.NodeSnapshot, bool)
```

The valid child must be a bound `read`/`fresh` child with nonempty Pi session ID, session file, and model fields and lifecycle `running`. Add negative cases for no child, multiple children, unbound child, wrong kind/context, and `ready`, `completed`, or `failed` lifecycle. This is stricter than general `isControllableChildSnapshot` because the managed control sequence requires observable active work, not merely a ready transport.

Add `TestFleetStreamContainsSessionEvaluatesIndividualPatches`. Feed in-memory complete Datastar SSE events where the first fleet patch lacks the descendant and the second contains exactly one `data-session-id="child-one"` marker; require a new helper:

```go
func fleetStreamContainsSession(r io.Reader, sessionID string) (bool, error)
```

Require false for a missing marker, a duplicate marker in one patch, a prefix-collision event type, and an empty session ID. No test may source `.env`, start a server, or require Incus despite the file's build tag.

- [ ] **Step 2: Prove RED for the missing test-only barriers**

Run:

```bash
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(ManagedDescendantFromTreeRequiresOneRunningBoundReadChild|FleetStreamContainsSessionEvaluatesIndividualPatches)$'
```

Expected: FAIL to compile only because `managedDescendantFromTree` and `fleetStreamContainsSession` do not exist.

- [ ] **Step 3: Implement exact running-child selection and reuse the bounded fleet-event scanner**

Modify only `internal/supervisor/live_incus_test.go`:

1. Implement `managedDescendantFromTree` to require exactly one direct child, require `isControllableChildSnapshot(child, contract.ChildKindRead, contract.ContextFresh)`, and additionally require lifecycle exactly `supervisor.LifecycleRunning`.
2. Refactor Task 6C's scanner body into a private `fleetStreamContainsPatch(r io.Reader, match func(string) bool) (bool, error)`. Preserve the exact `event: datastar-patch-elements` match, blank-line event boundaries, one-MiB token bound, immediate return, final nonempty event handling, and scanner-error propagation. Keep `fleetStreamContainsExactly` as a thin wrapper using `fleetPatchMatches`.
3. Implement `fleetStreamContainsSession` as another thin wrapper. Reject a blank session ID. A matching individual patch contains the exact `data-session-id="SESSION_ID"` marker once; zero or multiple occurrences reject that patch. Do not count across replacement patches.
4. Use exact equality for the SSE event type rather than a prefix match, so the prefix-collision regression fails closed.

- [ ] **Step 4: Keep the descendant observably active and wait for restarted-manager projection**

Modify only the managed acceptance flow and its private helpers:

1. Change `spawnManagedChild` to use `lifecycleActiveReadTask("KANEDIAS_E2E_MANAGED_DESCENDANT_OK")`, the already-reviewed bounded local-model workload used by the lifecycle control scenarios. It reads a fixed file list, forbids delegation and long-lived commands, and remains active long enough to expose a running child without sleeps.
2. Change `waitForManagedDescendant` to call `managedDescendantFromTree`; do not accept merely nonempty or terminal child snapshots. Preserve the existing bounded poll and direct root-socket observation.
3. Add `assertFleetContainsSession(client, serverOrigin, sessionID)`. Like `assertFleetContainsExactly`, use a five-second request context inside a 30-second `h.poll`, pass the response body to `fleetStreamContainsSession`, close it on every attempt, and accept only `matched && err == nil`.
4. Immediately after direct descendant discovery/tracking and before the first descendant server action, call `assertFleetContainsSession` against the restarted connection. Seeing the descendant row proves the manager's tree and routes were committed atomically; do not retry an action request and do not add a sleep.
5. Keep the existing descendant steer/interrupt/question/stop sequence unchanged in this task. If a later distinct control boundary fails, preserve its artifact and add another evidence-gated amendment rather than anticipating it here.

- [ ] **Step 5: Prove focused GREEN and race safety**

Run:

```bash
gofmt -w internal/supervisor/live_incus_test.go
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(ManagedDescendantFromTreeRequiresOneRunningBoundReadChild|FleetStreamContainsSessionEvaluatesIndividualPatches|FleetStreamContainsExactlyEvaluatesIndividualPatches|ParseManagedServerStartupModes|NormalizeManagedBootstrapURLToEffectiveOrigin|ManagedServerConnection(AuthenticatedAuthority|TrustedNoBootstrap))$'
go test -race -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(ManagedDescendantFromTreeRequiresOneRunningBoundReadChild|FleetStreamContainsSessionEvaluatesIndividualPatches|FleetStreamContainsExactlyEvaluatesIndividualPatches|ParseManagedServerStartupModes|NormalizeManagedBootstrapURLToEffectiveOrigin|ManagedServerConnection(AuthenticatedAuthority|TrustedNoBootstrap))$'
go test -count=1 -tags=incus ./internal/supervisor -run '^$'
git diff --check
```

Expected: running/binding selection and delayed manager projection pass; invalid child snapshots, missing/duplicate markers, and non-Datastar prefix collisions fail closed; all Task 6B/6C tests stay green normally and under the race detector; tagged compilation succeeds; `git diff --check` is silent.

- [ ] **Step 6: Rerun only the failed managed live acceptance**

Run:

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveServerManagedSupervisorAcceptance$' \
  -timeout 90m
```

Expected: PASS through the restarted-manager descendant projection barrier and all remaining controls, with exact process, socket, Incus instance, and volume baseline restoration. On a later failure, preserve the artifact and return to read-only diagnosis.

- [ ] **Step 7: Commit all jointly verified managed-acceptance hardening only after live GREEN**

Task 6B through 6D intentionally remain one uncommitted test-only change while each newly exposed live boundary is repaired and rerun. After focused and live GREEN:

```bash
git add internal/supervisor/live_incus_test.go
git commit -m "test: harden managed server acceptance"
```

Update the Task 6B/6C/6D reports with the shared commit and evidence, obtain independent implementation review, then resume Task 6 Step 2. Do not run the eight new lifecycle scenarios before both existing acceptances pass together.

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
