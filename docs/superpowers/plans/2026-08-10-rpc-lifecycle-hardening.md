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

Assert monotonic broker sequence, monotonic per-session source sequence, no duplicate `(sessionID, sourceSeq)`, and one non-overlapping `agent_start`/`agent_end` pair per observed generation unless that session is explicitly recorded as externally cancelled before final validation. External cancellation may truncate event forwarding after `agent_start`; an unclosed generation without exact cancellation evidence remains invalid. Treat `agent_settled` as an optional post-end confirmation in the shared validator because Pi may omit it for an aborted generation when queued work starts immediately; retain exact `agent_settled` totals in each scenario whose control contract requires them. Also require no remaining descendant socket, no owned process, and no proxy warning.

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

### Task 6E: Normalize the local E2E worker override to Pi's effective non-reasoning profile

**Failed invariant and evidence:**

- Required invariant: every managed/lifecycle child selected as `local-executor/Qwen3.6-27B-GGUF` must bind to the exact effective Pi profile before the local-model task runs; a simple local child failure must be diagnosed rather than dismissed.
- The Task 6D live rerun passed trusted restart and root fleet rediscovery, then timed out after eight minutes waiting for a running bound descendant. Retained artifacts are at `/home/steven/.cache/kanedias/e2e/e2e-3368639-1786395695044513541/`.
- `managed-logs/5b6ac72389c7e3396a64e0510c9d4698.log` records the exact pre-model failure: `effective Pi model ... ThinkingLevel:"off" does not match selected model ... ThinkingLevel:"xhigh"`. The asynchronous child POST returned normalized HTTP 500/internal, no child remained in the timeout Incus snapshot, and exact teardown restored the baseline.
- `assets/pi-models.json` declares this local model with `reasoning: false` and provider compatibility `supportsReasoningEffort: false`; Pi therefore correctly reports effective thinking `off`. The generated `managed-config.toml` instead selects reviewer `xhigh` because `writeManagedConfig` redirects provider/model while preserving each remote worker's original thinking and even broadens the local model's declared thinking levels to make that invalid selection pass config validation.
- Earliest divergent boundary: the test-only E2E config override constructs a selected profile the configured local Pi model cannot realize. `LocalSession.BindExpected` correctly enforces exact provider/model/thinking identity and must not be weakened. The model never received the child task, so this is not model noncompliance and no provider retry is appropriate.

**Files and scope:**

- Modify/Test: `internal/supervisor/live_incus_test.go`
- Modify/Test: `internal/supervisor/live_rpc_lifecycle_support_test.go`
- Read comparison only: `assets/pi-models.json`, `internal/config/model_policy.go`, and `internal/supervisor/local.go`
- Read artifacts: `/home/steven/.cache/kanedias/e2e/e2e-3368639-1786395695044513541/`
- Do not modify production binding invariants, Pi startup, model capability metadata, provider behavior, or any production file.

**Interfaces:**

- Consumes: the exact authorized local provider/model, its `off` effective thinking level, `writeManagedConfig`, and `validateLifecycleModelPolicy`.
- Produces: generated E2E worker policies that all resolve to `local-executor/Qwen3.6-27B-GGUF/off`, plus preflight validation of provider, model, and thinking before live side effects.

Independent review of this Task 6E amendment is required before Step 1 edits either test file.

- [ ] **Step 1: Add behavioral RED for local thinking normalization and preflight drift rejection**

Strengthen `TestWriteManagedConfigWorkerOverride` in `internal/supervisor/live_incus_test.go`:

1. Preserve and assert every administrator-owned worker description.
2. For both the existing matching local model and the added-model case, require every redirected worker to resolve to the requested provider/model with thinking exactly `off`, not its original remote `high`/`xhigh`.
3. Require the generated target model definition to have `thinking_levels == []string{"off"}` and `default_thinking_level == "off"`. This prevents catalog validation from advertising unsupported reasoning effort merely to preserve a prior worker setting.

Strengthen `TestValidateLifecycleModelPolicyRequiresLocalRootAndWorkers` in `internal/supervisor/live_rpc_lifecycle_support_test.go`. Change the helper contract to:

```go
func validateLifecycleModelPolicy(cfg config.Config, provider, model, thinkingLevel string) error
```

The valid policy is exact local/off for the root and worker. Keep the mixed-provider/model case, and add a catalog-valid same-provider/model worker at `xhigh`; require an error naming that worker and the thinking mismatch. Update the lifecycle setup call to expect `off`.

- [ ] **Step 2: Prove RED before implementation**

Run:

```bash
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(WriteManagedConfigWorkerOverride|ValidateLifecycleModelPolicyRequiresLocalRootAndWorkers)$'
```

Expected: FAIL because the current generated workers retain `high`/`xhigh` and `validateLifecycleModelPolicy` neither accepts nor checks the new exact-thinking argument. Do not weaken `BindExpected` to obtain GREEN.

- [ ] **Step 3: Generate one exact non-reasoning local worker profile**

Modify only `writeManagedConfig` and its test-only helpers/comments in `internal/supervisor/live_incus_test.go`:

1. Add a named test-only constant `const e2eLocalThinkingLevel = "off"` beside the managed acceptance helpers.
2. When both `KANEDIAS_E2E_WORKER_PROVIDER` and `KANEDIAS_E2E_WORKER_MODEL` are set, retain every worker description but set every worker's `thinking_level` to `off`.
3. Reuse an existing matching provider/model definition when present, but set its generated `thinking_levels` to exactly `[]string{"off"}` and `default_thinking_level` to `off`. When adding a model definition, use the same exact levels/default. Do not union prior worker levels or preserve an unrelated remote model's default.
4. Continue selecting one shared model type for all workers and preserving the unique provider/model invariant. Leave the root session's existing local/off selection unchanged.
5. Update comments so they no longer claim worker thinking is administrator-preserved during this explicit local E2E override. This normalization is test-only and applies only when both override environment variables are present.

- [ ] **Step 4: Fail lifecycle preflight on exact-thinking drift**

Modify only `internal/supervisor/live_rpc_lifecycle_support_test.go`:

1. Extend `validateLifecycleModelPolicy` to compare root provider, model, and thinking level with the exact expected triple.
2. Compare each sorted worker against the same triple and include the worker name in any mismatch.
3. Update `prepareLifecycleConfig` to call it with `"off"` for `local-executor/Qwen3.6-27B-GGUF`.
4. Do not infer that arbitrary nonempty thinking is acceptable; the checked asset/provider contract for this campaign is exact `off`.

- [ ] **Step 5: Prove focused GREEN and race safety**

Run:

```bash
gofmt -w internal/supervisor/live_incus_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(WriteManagedConfigWorkerOverride|ValidateLifecycleModelPolicyRequiresLocalRootAndWorkers|ManagedDescendantFromTreeRequiresOneRunningBoundReadChild|FleetStreamContainsSessionEvaluatesIndividualPatches|FleetStreamContainsExactlyEvaluatesIndividualPatches|ParseManagedServerStartupModes|NormalizeManagedBootstrapURLToEffectiveOrigin|ManagedServerConnection(AuthenticatedAuthority|TrustedNoBootstrap))$'
go test -race -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(WriteManagedConfigWorkerOverride|ValidateLifecycleModelPolicyRequiresLocalRootAndWorkers|ManagedDescendantFromTreeRequiresOneRunningBoundReadChild|FleetStreamContainsSessionEvaluatesIndividualPatches|FleetStreamContainsExactlyEvaluatesIndividualPatches|ParseManagedServerStartupModes|NormalizeManagedBootstrapURLToEffectiveOrigin|ManagedServerConnection(AuthenticatedAuthority|TrustedNoBootstrap))$'
go test -count=1 -tags=incus ./internal/supervisor -run '^$'
git diff --check
```

Expected: generated workers and lifecycle preflight require exact local/off; all Task 6B–6D tests remain green normally and under the race detector; tagged compilation succeeds; `git diff --check` is silent.

- [ ] **Step 6: Rerun only the failed managed live acceptance**

Run:

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveServerManagedSupervisorAcceptance$' \
  -timeout 90m
```

Expected: the local reviewer child binds as exact `local-executor/Qwen3.6-27B-GGUF/off`, reaches running, enters the restarted manager projection, and advances to all remaining controls with exact baseline restoration. If a later boundary fails, preserve artifacts and return to read-only diagnosis.

- [ ] **Step 7: Commit all jointly verified managed-acceptance hardening only after live GREEN**

Task 6B through 6E remain jointly uncommitted until the required live acceptance passes end to end. After focused/live GREEN and implementation review:

```bash
git add internal/supervisor/live_incus_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
git commit -m "test: harden managed server acceptance"
```

Update Task 6B/6C/6D/6E reports with the shared commit and evidence, then resume Task 6 Step 2. Do not run the eight new lifecycle scenarios before both existing acceptances pass together.

---

### Task 6F: Separate queued descendant steer from active root interrupt in managed acceptance

**Failed invariant and evidence:**

- Required invariant: the existing managed acceptance must prove server-routed descendant steer/question/stop and an active-session interrupt without accidentally turning the interrupt into an unbounded wait for a previously queued steer.
- The Task 6E live rerun proved exact local/off child binding and manager projection, then failed on the descendant interrupt after 26.22 seconds. Retained artifacts are at `/home/steven/.cache/kanedias/e2e/e2e-3387564-1786396592070742202/`.
- `managed-server-restart.log` proves the descendant fleet barrier completed, descendant steer succeeded in 1.07 milliseconds, and the immediately following interrupt failed exactly ten seconds later with `child_unavailable ... /rpc: context deadline exceeded`. The descendant POST later returned child failure during teardown; exact resource cleanup restored baseline.
- Pi 0.83's documented `steer` contract queues the message for delivery after the current assistant turn/tool batch. Its RPC implementation handles `abort` by awaiting `AgentSession.abort()`, which calls `agent.abort()` and then `waitForIdle()` without clearing pending steering/follow-up queues. A real standalone Pi/local-model probe matching the managed workload observed the acknowledged steer start another agent run and received no abort response within 90 seconds. The comparison probe with no queued steer observed streaming and received the abort response in 2 milliseconds with one `agent_end`/`agent_settled` pair.
- Earliest divergent boundary: `runServerManaged` sends steer and then abort back-to-back to the same transient child, conflating ordinary managed-control coverage with the dedicated `TestLiveRPCRapidControlLifecycle` contract. Raising the descendant unary timeout would only wait longer for queued work and would not test interruption isolation. This task must not weaken the ten-second availability bound or patch Pi semantics preemptively; the dedicated rapid-control scenario remains the evidence gate for that later behavior.

**Files and scope:**

- Modify/Test only: `internal/supervisor/live_incus_test.go`
- Read comparison only: Pi `docs/rpc.md`, Pi `dist/modes/rpc/rpc-mode.js`, Pi `dist/core/agent-session.js`, `internal/supervisorapi/client.go`, and `internal/manager/pi.go`
- Read artifacts: `/home/steven/.cache/kanedias/e2e/e2e-3387564-1786396592070742202/`
- Do not modify `DescendantClient` timeouts, production manager controls, Pi, the image, supervisor routing, or any production file.

**Interfaces:**

- Consumes: direct supervisor `get_state`, the existing restarted managed connection, `lifecycleActiveReadTask`, root persistence across server restart, and the server's steer/interrupt/question/stop routes.
- Produces: observable empty-queue/streaming barriers around an active interrupt of `roots[1]`, while the `roots[0]` descendant independently proves steer, controlled-question routing, and stop. Deep descendant interrupt remains covered by `TestLiveRPCInterruptLifecycle`; queued steer-to-abort behavior remains covered by `TestLiveRPCRapidControlLifecycle`.

Independent review of this Task 6F amendment is required before Step 1 edits `internal/supervisor/live_incus_test.go`.

- [ ] **Step 1: Add hermetic exact-state regressions for managed control barriers**

Add a pure helper and test:

```go
func managedSessionControlState(response map[string]any) (streaming bool, pending int, ok bool)
func TestManagedSessionControlStateRequiresExactSuccessfulState(t *testing.T)
```

Require a valid `get_state` response with `success=true`, object `data`, boolean `isStreaming`, and a finite nonnegative integral `pendingMessageCount`. Test both idle/zero and streaming/zero valid cases. Reject `success=false`, missing/non-object data, missing or non-boolean streaming, missing/fractional/negative/non-numeric pending count, and unrelated response shapes. The helper must not stringify or retain private response content.

- [ ] **Step 2: Prove RED for the missing state parser**

Run:

```bash
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestManagedSessionControlStateRequiresExactSuccessfulState$'
```

Expected: FAIL to compile because `managedSessionControlState` does not exist.

- [ ] **Step 3: Add observable idle/streaming barriers without sleeps or action retries**

Modify only `internal/supervisor/live_incus_test.go`:

1. Implement `managedSessionControlState` with exact dynamic-type checks for the JSON-decoded response. Convert `pendingMessageCount` only when it is a finite nonnegative integer representable as `int`.
2. Add `waitManagedSessionControlState(client, sessionID string, wantStreaming bool, wantPending int, description string)`. Use the existing bounded `h.poll` and direct supervisor `get_state`; accept only an exact parsed state matching both requested values. Do not log raw responses.
3. Add `exerciseManagedRootInterrupt(connection managedServerConnection, root managedRoot)`. First wait for the persistent root to be idle with zero pending messages. Send exactly one server steer action while idle with `lifecycleActiveReadTask("KANEDIAS_E2E_MANAGED_ROOT_INTERRUPT")`; `Manager.SendMessage` therefore emits a Pi `prompt`, not a queued `steer`. Wait for direct `get_state` to report streaming with zero pending messages. Send the server interrupt exactly once, then wait for idle with zero pending messages. This proves an active interrupt through the restarted server without queued work.
4. Keep all polls bounded and state-based; do not add time sleeps and do not retry steer/interrupt POSTs.

- [ ] **Step 4: Assign independent controls to stable sessions**

In `runServerManaged`, after restarted root fleet rediscovery and before descendant creation, call `exerciseManagedRootInterrupt` for `roots[1]`; `roots[0]` remains the separate descendant owner. Then retain the descendant creation, direct running-child barrier, manager fleet projection barrier, descendant steer, controlled-question answer, and descendant stop. Remove only the immediately-following descendant interrupt call and update comments to describe the split coverage. Root stop and exact cleanup remain unchanged. `exerciseManagedRootInterrupt` ends at an idle/zero-pending barrier before the test proceeds.

The descendant's controlled question is triggered by an extension command; Pi's RPC contract executes extension commands immediately even during streaming, so it remains independent of the queued steer and can be answered before descendant stop.

- [ ] **Step 5: Prove focused GREEN and race safety**

Run:

```bash
gofmt -w internal/supervisor/live_incus_test.go
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(ManagedSessionControlStateRequiresExactSuccessfulState|WriteManagedConfigWorkerOverride|ValidateLifecycleModelPolicyRequiresLocalRootAndWorkers|ManagedDescendantFromTreeRequiresOneRunningBoundReadChild|FleetStreamContainsSessionEvaluatesIndividualPatches|FleetStreamContainsExactlyEvaluatesIndividualPatches|ParseManagedServerStartupModes|NormalizeManagedBootstrapURLToEffectiveOrigin|ManagedServerConnection(AuthenticatedAuthority|TrustedNoBootstrap))$'
go test -race -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(ManagedSessionControlStateRequiresExactSuccessfulState|WriteManagedConfigWorkerOverride|ValidateLifecycleModelPolicyRequiresLocalRootAndWorkers|ManagedDescendantFromTreeRequiresOneRunningBoundReadChild|FleetStreamContainsSessionEvaluatesIndividualPatches|FleetStreamContainsExactlyEvaluatesIndividualPatches|ParseManagedServerStartupModes|NormalizeManagedBootstrapURLToEffectiveOrigin|ManagedServerConnection(AuthenticatedAuthority|TrustedNoBootstrap))$'
go test -count=1 -tags=incus ./internal/supervisor -run '^$'
git diff --check
```

Expected: exact state parsing and both barriers pass; all Task 6B–6E tests remain green normally and under the race detector; tagged compilation succeeds; no production file changes; `git diff --check` is silent.

- [ ] **Step 6: Rerun only the failed managed live acceptance**

Run:

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveServerManagedSupervisorAcceptance$' \
  -timeout 90m
```

Expected: active root interrupt acknowledges with an empty queue; descendant steer/question/stop all succeed independently; roots stop; exact process/socket/Incus baseline is restored. Preserve and diagnose any later boundary rather than broadening this change.

- [ ] **Step 7: Commit jointly verified managed-acceptance hardening only after live GREEN**

After focused/live GREEN and implementation review:

```bash
git add internal/supervisor/live_incus_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
git commit -m "test: harden managed server acceptance"
```

Update Task 6B through 6F reports with the shared commit and evidence, then resume Task 6 Step 2. Both the dedicated descendant-interrupt and rapid-control scenarios must still run in Task 6 Step 3 and may not borrow this test-only separation as evidence of their correctness.

---

### Task 6G: Linearize direct-child cancellation and publish typed abort before child teardown

**Failed invariants and evidence:**

- Required direct-stop invariant: after an accepted direct-child `DELETE`, the in-flight create call returns exact `child_aborted`/HTTP 409, the child exits cleanly, and the root remains usable. `/home/steven/.cache/kanedias/e2e/e2e-3420990-1786397870366777991/` instead returned `child_failed`/HTTP 502 at terminal acknowledgement.
- Required root-end invariant: stopping a root with three active children returns each create call as `child_aborted`, exits the root zero, and restores the exact four-session baseline. `/home/steven/.cache/kanedias/e2e/e2e-3420990-1786397881990136240/` instead recorded three cancelled child runtimes and root exit status 1.
- Required abort invariant: routed Pi `abort` acknowledges while the child server remains available, the child emits one aborted terminal generation, and the create call returns `child_aborted`. `/home/steven/.cache/kanedias/e2e/e2e-3420990-1786397894750893548/` and the abort sibling in `/home/steven/.cache/kanedias/e2e/e2e-3420990-1786398052076315968/` both contain the correct Pi order `message_end(stopReason=aborted) -> agent_end -> agent_settled`, but the routed abort lost the child transport and the create call later returned `child_failed` while attempting terminal acknowledgement.
- Earliest abort divergence: `productionChildRunnerWithRuntime` returns the `RunReadTask` error and executes its deferred `node.Stop` before `RunInheritedChild` can publish `reporter.Failure`. The child Unix/SSE server therefore closes before the in-flight abort response and before the direct parent can ingest and acknowledge the typed terminal report.
- Earliest requested-stop divergence: cancellation deliberately closes the terminal-ack endpoint, but `childEntry` has no external-cancellation versus terminal-acceptance linearization. `CreateChild` can ingest a terminal message or EOF after cancellation and still attempt acknowledgement or classify the failure as `child_failed`. When a descendant client exists, cleanup requests child HTTP stop without closing the inherited parent-liveness endpoint that is the architecture's canonical cancellation signal.
- The terminal-ack errors after failed abort routing are secondary cleanup fallout, not evidence that Pi ignored abort. Do not increase timeouts, retry controls, or weaken the `child_aborted` contract.

**Files:**

- Modify/Test: `cmd/session_runtime.go`, `cmd/session_runtime_test.go`
- Modify/Test: `internal/supervisor/result.go`, `internal/supervisor/read_result_test.go`
- Modify/Test: `internal/supervisor/children.go`, `internal/supervisor/children_test.go`
- Modify/Test: `internal/supervisor/node.go`, `internal/supervisor/node_test.go`
- Modify/Test if required by the direct-owner seam: `internal/supervisor/router.go`, `internal/supervisor/router_test.go`
- Read comparison: `internal/supervisor/process/liveness.go`, `internal/supervisor/process/protocol.go`, `internal/supervisor/process/spawn.go`, and `docs/architecture/session-supervisor.md`

**Interfaces and non-negotiable ordering:**

- A successfully validated terminal report still linearizes before cancellation, marks descendant SSE closure expected, receives exactly one acknowledgement byte, and only then begins normal cleanup.
- External cancellation still closes the terminal-ack endpoint without acknowledgement and closes inherited parent liveness so the child recursively cancels its subtree.
- Generic failure cleanup must never mark a child as externally cancelled; real startup/runtime failures remain `child_failed`.
- A typed non-cancellation read failure is published and acknowledged while the child supervisor transport is still live. Inherited-context cancellation publishes no terminal report.

Independent review of this Task 6G amendment is required before Step 1 changes production code.

- [ ] **Step 1: Add narrow RED regressions for all three race boundaries**

Add hermetic tests proving:

1. `RunReadTask` returns the already-cancelled inherited context rather than `child_failed` when RPC EOF races that cancellation.
2. A production read-child aborted/error result publishes one privacy-safe typed `MessageFailure` and waits for parent acknowledgement before `CloseListener`/deferred node teardown; when the inherited context is cancelled, no terminal report is published.
3. An external direct-child cancellation that wins before terminal acceptance closes ack and liveness, produces no acknowledgement, waits for process cleanup, and resolves the blocked `CreateChild` as `child_aborted` rather than `child_failed`.
4. A valid terminal report that wins the same race is acknowledged exactly once and retains its original success/failure result; a genuine child failure through generic cleanup remains `child_failed`.
5. Root stop applies the external-cancellation path concurrently to all registered children without a deadlock, nonzero cleanup error, or residual entry.

Use explicit channels for cancellation admission, terminal-message admission, acknowledgement, liveness close, and process completion. Do not use sleeps as the ordering seam.

- [ ] **Step 2: Prove RED before implementation**

Run the exact new test names selected in Step 1 under their owning packages. Expected: compile failures for the missing child-entry cancellation/terminal-claim seams and/or assertion failures showing teardown precedes failure publication and cancellation becomes `child_failed`. Record the exact RED commands and output in the Task 6G report before editing production.

- [ ] **Step 3: Publish read failures before the child runtime defer tears down transport**

In `cmd/session_runtime.go`, add one private helper used by the read-child branch:

1. If `RunReadTask` succeeds, preserve `reporter.Read` exactly.
2. If the inherited context is cancelled, return the context error without publishing a terminal report; parent cancellation already closed the ack endpoint.
3. Otherwise map a typed `contract.Error` to its existing code/message, map an untyped error to fixed `internal`/`internal supervisor error`, call `reporter.Failure`, and return `errors.Join(runErr, reportErr)`. `Reporter.TerminalSent` prevents `RunInheritedChild` from publishing a duplicate.
4. Because `reporter.Failure` waits for the direct-parent acknowledgement, the existing deferred `node.Stop` cannot close Unix/SSE before terminal ingestion. Parent-liveness cancellation must remain able to unblock that wait; do not add a timeout or detached goroutine.

In `internal/supervisor/result.go`, when `local.rpc.Done()` wins, return `ctx.Err()` if the inherited context is already cancelled before classifying an unexpected RPC EOF as `child_failed`.

- [ ] **Step 4: Add a direct-child cancellation versus terminal-acceptance linearization**

Add monotonic state and small locked methods on `childEntry` so exactly one boundary wins:

1. An external cancellation origin claims cancellation only if terminal acceptance has not already been claimed.
2. Terminal acceptance claims only if external cancellation has not already been claimed; claiming terminal acceptance also marks descendant SSE closure expected before acknowledgement.
3. Generic `failChildCreation` cleanup does not claim external cancellation.

Add a dedicated parent-owned cancellation helper. Use it from direct-target `Router.Stop` and `stopChildren`; deeper targets continue routing to the direct owner, which applies the same rule. If cancellation wins, close terminal ack and inherited liveness as the canonical non-acknowledged cancellation signal, mark/cancel event ownership, then bound process wait/escalation/recovery/removal with the existing cleanup deadline. Do not wait for a descendant HTTP stop before liveness EOF, and do not turn an expected closed child server into a cleanup failure.

In `CreateChild`, check the linearized outcome after `NextMessage` failure and at the terminal-acceptance boundary. If cancellation won, do not acknowledge any terminal report; wait for the one existing cleanup operation with the normal bounded child-stop context and return exact `child_aborted`. If terminal acceptance won, retain all existing identity/kind/worker checks, exact acknowledgement, and normal cleanup. Cover the residual cancellation-versus-ack error race by consulting the same linearized outcome rather than rewriting unrelated acknowledgement failures.

- [ ] **Step 5: Prove focused GREEN and race safety**

Run:

```bash
gofmt -w cmd/session_runtime.go cmd/session_runtime_test.go \
  internal/supervisor/result.go internal/supervisor/read_result_test.go \
  internal/supervisor/children.go internal/supervisor/children_test.go \
  internal/supervisor/node.go internal/supervisor/node_test.go \
  internal/supervisor/router.go internal/supervisor/router_test.go
go test -v -count=1 ./cmd ./internal/supervisor -run '<exact Task 6G regression alternation>'
go test -race -v -count=1 ./cmd ./internal/supervisor -run '<exact Task 6G regression alternation>'
go test -count=1 ./cmd ./internal/supervisor
git diff --check
```

If no router production/test edit is required, omit those paths from `gofmt`; do not create placeholder files. Expected: all new comparisons pass normally and under race, existing child terminal-ordering/cancellation tests remain green, and diff check is silent.

- [ ] **Step 6: Rerun only the four affected live scenarios once**

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveRPC(ChildStop|RootEnd|Interrupt|MixedSibling)Lifecycle$' \
  -timeout 2h
```

Expected: direct stop and root cascade return exact `child_aborted`; root exit is zero; root and child aborts acknowledge over live transports; mixed natural/delete/abort siblings retain independent outcomes; exact process/socket/Incus baseline is restored. A later failure gets a new artifact and plan amendment rather than a broadened fix.

- [ ] **Step 7: Review and commit**

After focused and live GREEN, obtain independent implementation review, then commit only the verified Task 6G production/tests:

```bash
git add cmd/session_runtime.go cmd/session_runtime_test.go \
  internal/supervisor/result.go internal/supervisor/read_result_test.go \
  internal/supervisor/children.go internal/supervisor/children_test.go \
  internal/supervisor/node.go internal/supervisor/node_test.go \
  internal/supervisor/router.go internal/supervisor/router_test.go
git commit -m "fix: linearize child cancellation teardown"
```

Stage only files that actually changed.

---

### Task 6H: Validate Pi generations by `agent_end` while treating `agent_settled` as optional confirmation

**Failed invariant and evidence:**

- `/home/steven/.cache/kanedias/e2e/e2e-3420990-1786398020084033881/` completed rapid steer -> abort -> isolated follow-up and exact cleanup, then `validateLifecycleFinalEvents` rejected a new `agent_start` because the prior generation lacked `agent_settled`.
- The complete source order is: generation 1 `agent_start -> message_end(aborted) -> agent_end` with no settled event; generation 2 `agent_start -> message_end(stop) -> agent_end -> agent_settled`; generation 3 has the same complete order. Starts and ends are exactly 3/3 and never overlap.
- Earliest divergence is test-only: `validateLifecycleFinalEvents` closes an open generation only on `agent_settled`, but Pi's run boundary is `agent_end`; `agent_settled` is a later confirmation that may be omitted for an aborted generation when queued work immediately starts another run. Existing scenario-specific settlement totals remain authoritative where exact settled counts are part of the control contract.

**Files and scope:**

- Modify/Test only: `internal/supervisor/live_rpc_lifecycle_support_test.go`
- Do not modify Pi, event forwarding, control semantics, or scenario-specific settlement assertions.

Independent review of this Task 6H amendment is required before implementation.

- [ ] **Step 1: Add a RED event-order regression**

Rewrite the existing valid fixture in `TestValidateLifecycleFinalEventsRequiresOrderedPairedGenerations` so every generation contains `agent_start -> agent_end`, with selected generations followed by `agent_settled`; the fixture must include one ended generation whose optional settlement is omitted before a later `agent_start`. Re-derive every mutation index against this new fixture rather than retaining edits that target the old start/settled-only positions. Require acceptance of a sequence containing `agent_start -> agent_end -> agent_start -> agent_end -> agent_settled`. Add invalid comparisons for end without start, overlapping start before end, unclosed start, settled while a generation is open, and duplicate settled confirmation for one ended generation. Keep broker/source ordering and uniqueness cases.

Run:

```bash
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestValidateLifecycleFinalEventsRequiresOrderedPairedGenerations$'
```

Expected RED: the valid missing-settled sequence is rejected as overlapping.

- [ ] **Step 2: Close generations on `agent_end` and validate optional settlement locally**

Change only `validateLifecycleFinalEvents`:

1. `agent_start` opens a generation and clears any unconsumed settlement eligibility from an earlier ended generation.
2. `agent_end` requires one open generation, closes it, and makes that ended generation eligible for at most one `agent_settled` confirmation.
3. `agent_settled` is allowed only with no open generation and one current eligible ended generation, then consumes eligibility.
4. Final validation requires no open generation unless a later evidence-gated amendment supplies exact external-cancellation identity for that session; it does not require every ended generation to settle.
5. Preserve strict broker sequence, source sequence, source identity, and JSON parsing behavior.

Do not weaken `validateLifecycleSettlementTotals`, `waitLifecycleSettlement`, or scenario-specific exact settlement counts.

- [ ] **Step 3: Prove GREEN and rerun rapid control**

```bash
gofmt -w internal/supervisor/live_rpc_lifecycle_support_test.go
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestValidateLifecycleFinalEventsRequiresOrderedPairedGenerations$'
go test -race -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestValidateLifecycleFinalEventsRequiresOrderedPairedGenerations$'
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveRPCRapidControlLifecycle$' -timeout 90m
git diff --check
```

Expected: hermetic/race/live GREEN with exact cleanup and the scenario's existing two-settlement total unchanged.

- [ ] **Step 4: Review and commit**

After independent implementation review:

```bash
git add internal/supervisor/live_rpc_lifecycle_support_test.go
git commit -m "test: validate lifecycle generations by agent end"
```

---

### Task 6I: Preserve strict deterministic-child provenance with concise model-facing markers

**Failed invariant and evidence:**

- `/home/steven/.cache/kanedias/e2e/e2e-3420990-1786397593102734287/` returned a healthy HTTP 200 typed reviewer/read result for the observed child session after real repository reads, but its assistant output dropped one character from the requested long marker (`LIFECYCLE` became a one-character transcription variant).
- Prompt bytes, event delivery, JSON encoding, and the assertion all contain the same correct ASCII marker. The first divergence is the local model's terminal echo, not supervisor transport or stale output.
- Exact run provenance remains required. Do not add edit distance, case folding, retry, or a fallback that accepts output lacking the exact run prefix. Reduce the model-facing lexical burden instead.

**Files and scope:**

- Modify/Test only: `internal/supervisor/live_rpc_lifecycle_test.go`
- Do not modify the provider/model, production child result contract, or exact `strings.Contains` assertion.

Independent review of this Task 6I amendment is required before implementation.

- [ ] **Step 1: Add a RED concise-marker regression**

Add `TestLifecycleMarkerIsConciseExactAndRunScoped` for a new pure helper. Require exact outputs such as `KDS_DS_e2e-run` and `KDS_DP_2_e2e-run`, no spaces, and retention of the complete supplied run prefix. The helper is missing, so the focused test must fail to compile before implementation.

- [ ] **Step 2: Use compact codes without weakening exactness**

Implement the pure marker constructor and use it for deterministic direct single/parallel child markers only: `DS` for direct single and `DP_<index>` for direct parallel. Continue passing the exact marker in each task and requiring byte-exact containment in `assertLifecycleReadResult`. Keep unique child identity, real read workload, output summary, natural completion, and root-usability assertions unchanged.

- [ ] **Step 3: Prove GREEN and rerun deterministic children**

```bash
gofmt -w internal/supervisor/live_rpc_lifecycle_test.go
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLifecycleMarkerIsConciseExactAndRunScoped$'
go test -race -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLifecycleMarkerIsConciseExactAndRunScoped$'
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveRPCDeterministicChildLifecycle$' -timeout 90m
git diff --check
```

Expected: strict marker assertion passes without fuzzy matching; the one-single/three-parallel topology, typed results, exact identities, natural cleanup, and baseline restoration all pass.

- [ ] **Step 4: Review, commit, and resume the complete matrix**

```bash
git add internal/supervisor/live_rpc_lifecycle_test.go
git commit -m "test: shorten lifecycle provenance markers"
```

After Tasks 6G–6I are independently live GREEN, rerun all eight scenarios once. Only a complete clean matrix may proceed to Task 7; any code change or failure resets the affected scenario's five-run count.

---

### Task 6J: Admit truncated generations only for explicitly cancelled sessions

**Failed invariant and evidence:**

- After Task 6G, `/home/steven/.cache/kanedias/e2e/e2e-3496270-1786399903873442764/` returns exact 409 `child_aborted`, removes the child, proves the root usable, exits cleanly, and restores baseline; only final validation rejects the cancelled child's `agent_start` without `agent_end`.
- `/home/steven/.cache/kanedias/e2e/e2e-3496270-1786399926571384197/` likewise returns exact 409 `child_aborted` for all three root-cascade children and reaches final validation; each cancelled child ends its forwarded stream after the user message.
- In both corresponding pre-Task-6G artifacts, externally stopped children already lacked `agent_end`/`agent_settled`; Task 6G did not remove those events. `cleanupChild` marks descendant stream closure expected and cancels forwarding as part of parent-owned cancellation, so a forced cancellation intentionally does not promise a complete Pi terminal stream.
- `/home/steven/.cache/kanedias/e2e/e2e-3496270-1786400019776431893/` proves outcome separation: abort child has `message_end(aborted) -> agent_end -> agent_settled` and returns 409; natural child has `message_end(stop) -> agent_end -> agent_settled` and returns 200; external DELETE child returns 409 but has no settlement. The timeout waiting for one DELETE settlement and the shared no-open-generation rule are test-contract defects.
- Do not globally accept unclosed generations. Only a session recorded from an accepted external `DELETE` (including descendants present in the root tree immediately before root DELETE) may end with an open generation.

**Files and scope:**

- Modify/Test only: `internal/supervisor/live_rpc_lifecycle_support_test.go`, `internal/supervisor/live_rpc_lifecycle_test.go`
- Do not modify Task 6G production cancellation, event forwarding, Pi, or settlement behavior.

Independent review of this Task 6J amendment is required before implementation.

- [ ] **Step 1: Add RED cancellation-evidence regressions**

Change the pure validator contract to accept an explicit set of externally cancelled session IDs. Extend `TestValidateLifecycleFinalEventsRequiresOrderedPairedGenerations` with the same unclosed `agent_start` sequence twice: it must remain invalid with an empty allowed set and become valid only when that exact session ID is in the allowed set. A different allowed ID must not help. Keep every existing ordering/end/settlement invalid case.

Add a pure test for recording a cancellation tree: recording a direct child includes only that target; recording a root from a current tree includes the root and every currently projected descendant; empty/unknown IDs are not added accidentally. The RED must be a compile failure for the missing explicit-cancellation seams or a focused assertion failure against the unchanged validator.

- [ ] **Step 2: Record exact external cancellation identity at the action boundary**

In `lifecycleRoot`, add a mutex-protected set of externally cancelled session IDs and initialize it with the other root evidence state. Immediately before `deleteLifecycleSession` sends a request:

1. For a non-root target, record exactly that session ID.
2. For root DELETE, read the current root tree and record the root plus every descendant present in that snapshot. A failed snapshot records only the root; any unrecorded open descendant still fails final validation rather than being broadly excused.
3. A rejected/transport-failed DELETE still fails its scenario at the existing status assertion, so the evidence cannot mask an unsuccessful control.
4. Do not record Pi `abort`, natural completion, steer, or root-usability prompts as external cancellation.

Pass an immutable copy of this set to `validateLifecycleFinalEvents` at the fully drained final boundary. The validator still closes generations on `agent_end`; at EOF it permits an open generation only when that exact session is externally cancelled. Ordering, uniqueness, end-without-start, overlap, and settled-state validation remain strict for all sessions.

- [ ] **Step 3: Remove the nonexistent mixed-DELETE settlement promise without weakening siblings**

In `exerciseMixedSiblingOutcomes`, retain exact 409 `child_aborted`, identity-specific disappearance, process/socket/resource cleanup, and final cancellation evidence for the DELETE child. Remove only `waitLifecycleSettlementTotal(...deleteChild..., 1)` and omit the DELETE child from `assertLifecycleSettlementTotals`. Continue requiring exactly one settlement for the abort child and natural child, exact natural output, immutable natural events after sibling controls, and root settlement.

- [ ] **Step 4: Prove focused GREEN/race and rerun affected live scenarios**

```bash
gofmt -w internal/supervisor/live_rpc_lifecycle_support_test.go internal/supervisor/live_rpc_lifecycle_test.go
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(ValidateLifecycleFinalEventsRequiresOrderedPairedGenerations|LifecycleExternalCancellationEvidence)$'
go test -race -v -count=1 -tags=incus ./internal/supervisor \
  -run '^Test(ValidateLifecycleFinalEventsRequiresOrderedPairedGenerations|LifecycleExternalCancellationEvidence)$'
go test -count=1 -tags=incus ./internal/supervisor -run '^$'
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveRPC(ChildStop|RootEnd|MixedSibling)Lifecycle$' -timeout 2h
git diff --check
```

Expected: externally cancelled open generations are admitted only by exact identity; unmarked truncation remains invalid; mixed abort/natural settlement contracts remain exact; all three live scenarios pass with baseline restoration.

- [ ] **Step 5: Review and commit**

After independent implementation review:

```bash
git add internal/supervisor/live_rpc_lifecycle_support_test.go internal/supervisor/live_rpc_lifecycle_test.go
git commit -m "test: identify externally cancelled generations"
```

---

### Task 6K: Make post-abort root usability explicitly supersede the aborted task

**Failed invariant and evidence:**

- `/home/steven/.cache/kanedias/e2e/e2e-3496270-1786399939127754928/` proves root abort itself succeeded with `message_end(aborted) -> agent_end -> agent_settled`. The following user message containing the exact root-usability marker was delivered in a new `agent_start` generation.
- Instead of obeying the short `Reply with exactly ...` message, the local model resumed the earlier aborted twelve-file analysis, issued repository tool calls, and produced a long response without the marker. This is a real local-model instruction-priority failure after abort, not a transport, binding, or tool-execution failure and not stale event replay.
- The usability probe must explicitly state that the prior task was aborted and must not be resumed, prohibit tools, and retain a byte-exact run marker. Do not retry, fuzzy-match, hide the failure, or weaken the existing final-text assertion.

**Files and scope:**

- Modify/Test only: `internal/supervisor/live_rpc_lifecycle_test.go`
- Do not modify Pi, provider/model, abort behavior, RPC routing, timeouts, or generic non-abort usability probes.

Independent review of this Task 6K amendment is required before implementation.

- [ ] **Step 1: Add a RED pure prompt regression**

Add `TestLifecyclePostAbortProbeExplicitlySupersedesPriorTask` for a missing pure helper. Require the prompt to contain the exact supplied marker exactly once and explicit fixed instructions that the previous request was aborted, must not be resumed, no tools may be called, and the response must contain only the marker. Reject marker omission/duplication in the constructed string. The focused test must fail to compile before the helper exists.

- [ ] **Step 2: Use the specialized strict probe only after root abort**

Extract the common settlement/final-text/state logic from `assertRootUsable` into a private helper that accepts the prompt while retaining the expected marker. Preserve the generic `Reply with exactly MARKER.` wrapper for every existing non-abort caller. Add a post-abort wrapper using the new pure prompt and call it only after the root interrupt settlement in `exerciseLifecycleInterrupt`. Keep one prompt, one bounded settlement wait, exact `strings.Contains` final-text check, and typed idle-state check. Do not add retries, sleeps, alternative markers, or tool-output acceptance.

- [ ] **Step 3: Prove focused GREEN/race and rerun interrupt once**

```bash
gofmt -w internal/supervisor/live_rpc_lifecycle_test.go
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLifecyclePostAbortProbeExplicitlySupersedesPriorTask$'
go test -race -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLifecyclePostAbortProbeExplicitlySupersedesPriorTask$'
go test -count=1 -tags=incus ./internal/supervisor -run '^$'
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveRPCInterruptLifecycle$' -timeout 90m
git diff --check
```

Expected: root and child abort each retain exact aborted/end/settled semantics; the specialized fresh generation returns the exact marker without resuming the old task; child create returns 409 `child_aborted`; root remains usable; exact baseline is restored.

- [ ] **Step 4: Review, commit, and resume the one-pass matrix**

```bash
git add internal/supervisor/live_rpc_lifecycle_test.go
git commit -m "test: clarify post-abort usability probe"
```

After Tasks 6J and 6K pass live, rerun Task 6H rapid control and Task 6I deterministic children if they have not yet passed on the final code, then rerun all eight scenarios once. Any later failure receives another evidence-gated amendment.

---

### Task 6L: Quiesce admitted child RPC handlers before terminal report acknowledgement

**Failed invariant and evidence:**

- After Tasks 6G/6K, `/home/steven/.cache/kanedias/e2e/e2e-3539447-1786400920601429667/` proves the root post-abort probe succeeded and the deep child reached streaming. Pi then emitted exact child `message_end(stopReason=aborted) -> agent_end -> agent_settled`, and the asynchronous child-create call returned exact 409 `child_aborted` with message `read child was aborted`.
- The routed child `abort` request itself still returned normalized 502 `child_unavailable ... internal supervisor error`. The child root log contains the typed aborted result followed by process exit 1; cleanup restored the exact Incus baseline.
- Task 6G correctly moved typed failure publication before deferred `node.Stop`, but `reporter.Failure` may be acknowledged by the direct parent before the child supervisor's already-admitted `Node.CallRPC(abort)` handler has received and returned Pi's response. The child runtime then leaves `reporter.Failure`, runs deferred `node.Stop`, and closes Pi RPC underneath that handler. Which path wins is timing-dependent: the mixed-sibling abort passed on the same code while this isolated deep abort failed.
- Earliest divergence is therefore terminal publication versus completion of an already-admitted child RPC handler, not Pi abort, terminal classification, descendant routing, model compliance, timeout length, or cleanup. Do not retry abort, delay acknowledgement, inflate the ten-second descendant timeout, or special-case a 502 as success.

**Files:**

- Modify/Test: `internal/supervisor/node.go`, `internal/supervisor/node_test.go`
- Modify/Test: `cmd/session_runtime.go`, `cmd/session_runtime_test.go`
- Read comparison only: `internal/supervisorapi/handler.go`, `internal/supervisorapi/client.go`, `internal/supervisor/process/protocol.go`

**Interfaces and ordering:**

- `Node.CallRPC` admits or rejects each routed RPC under a private gate, counts admitted calls until their exact return, and preserves all existing binding/command/error behavior.
- Child terminal quiescence atomically closes new RPC admission and waits for every already-admitted call to return. It is used only by the read-child terminal path, not explicit parent cancellation.
- For abort, the already-admitted handler returns its exact successful Pi response before the child publishes `MessageFailure(child_aborted)`. The direct parent can then acknowledge, after which normal child teardown begins.
- Inherited-context cancellation skips terminal quiescence/publication and retains Task 6G's ack/liveness cancellation path.

Independent review of this Task 6L amendment is required before Step 1 edits production.

- [ ] **Step 1: Add channel-coordinated RED RPC-quiescence regressions**

In `internal/supervisor/node_test.go`, add a test with a bound fake local RPC peer and explicit channels:

1. Start one admitted `Node.CallRPC` and hold its matching Pi response.
2. Begin terminal quiescence in another goroutine and prove it does not return while that call is active.
3. Prove a new RPC after quiescence admission closes is rejected with exact `session_stopping` without reaching the fake peer.
4. Release the original response; require that call to return exact success and quiescence to return nil.
5. Repeated/concurrent quiescence callers observe the same closed admission and completed drain without panic or reopen.

In `cmd/session_runtime_test.go`, extend the Task 6G read-failure ordering regression so an admitted abort-like `Node.CallRPC` remains blocked when `RunReadTask` observes aborted settlement. Require no terminal failure report until that RPC response is released; then require exact RPC success, one typed `MessageFailure(child_aborted)`, parent acknowledgement, and only afterward listener/runtime teardown. Add the success-result comparison if needed to prove natural read results use the same quiescence boundary. Use channels, never sleeps, as the ordering seam.

- [ ] **Step 2: Prove RED before implementation**

Run the exact new focused tests. Expected RED: compile failure for the missing quiescence seam or an ordering assertion showing the terminal failure report appears while the admitted RPC remains blocked. Record the exact command/output before production edits.

- [ ] **Step 3: Add a monotonic per-node RPC admission/drain gate**

In `Node`, add a private mutex-protected RPC gate with:

1. a boolean permanently closing new admission for terminal teardown;
2. an exact active-call count; and
3. an idle notification channel created on the zero-to-one transition and closed on the one-to-zero transition.

`Node.CallRPC` must preserve its current lifecycle/local checks, then atomically reject closed admission with `contract.ErrorSessionStopping` or increment the count, and always decrement in a defer after `local.CallRPC` returns. No `sync.WaitGroup` Add/Wait race is permitted.

Add an exported child-runtime method with a narrow name such as `QuiesceRPC(ctx) error`. It atomically closes admission, returns immediately when no call is active, or snapshots the current idle channel, releases the gate mutex, and only then waits for idle/context so the in-flight call's deferred decrement can reacquire the mutex and close the channel. Repeated callers are idempotent. It must not stop Pi, close the Unix listener, mutate binding/model identity, or reopen admission.

- [ ] **Step 4: Quiesce before publishing any read-child terminal report**

In `productionChildRunnerWithRuntime` read handling:

1. If inherited context cancellation won, retain Task 6G behavior: publish nothing and return for parent-owned cancellation.
2. Otherwise create a bounded cleanup context from `context.WithoutCancel(ctx)` using the existing child runtime cleanup bound and call `node.QuiesceRPC` before `reporter.Read` or `publishReadFailure`.
3. On a normal read result, a quiescence failure must not publish success; publish one fixed privacy-safe internal failure instead and return the joined diagnostics.
4. On an existing typed read failure such as `child_aborted`, quiesce first, then publish that same typed code/message; retain any quiescence error only in joined local diagnostics.
5. The existing deferred `node.Stop` remains after terminal report acknowledgement. `Reporter.TerminalSent` still prevents duplicate outer publication.

Do not change top-level root RPC semantics, explicit cancellation, reporter acknowledgement bytes, supervisor API timeout, or error normalization.

- [ ] **Step 5: Prove focused/full GREEN and race safety**

```bash
gofmt -w internal/supervisor/node.go internal/supervisor/node_test.go \
  cmd/session_runtime.go cmd/session_runtime_test.go
go test -v -count=1 ./internal/supervisor ./cmd \
  -run '<exact Task 6L RPC quiescence regression alternation>'
go test -race -v -count=1 ./internal/supervisor ./cmd \
  -run '<exact Task 6L RPC quiescence regression alternation>'
go test -count=1 ./internal/supervisor ./cmd
go test -race -count=1 ./internal/supervisor ./cmd
go vet ./internal/supervisor ./cmd
git diff --check
```

Expected: admitted-call response precedes terminal report, new calls fail closed, repeated drains are safe, Task 6G cancellation/failure tests remain green, full packages pass normally and under race, and diff check is silent.

- [ ] **Step 6: Rerun interrupt and mixed abort once**

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveRPC(Interrupt|MixedSibling)Lifecycle$' -timeout 2h
```

Expected: both root/child abort acknowledgements return exact success before child terminal 409; mixed natural/delete/abort outcomes stay independent; all terminal/action/event/resource invariants pass; exact baseline is restored.

- [ ] **Step 7: Review, commit, and resume remaining live gates**

After independent implementation review:

```bash
git add internal/supervisor/node.go internal/supervisor/node_test.go \
  cmd/session_runtime.go cmd/session_runtime_test.go
git commit -m "fix: drain child RPC before terminal teardown"
```

Then rerun Task 6H rapid control and Task 6I deterministic children on the final code, followed by all eight scenarios once. Any failure receives another evidence-gated amendment.

---

### Task 6M: Prove post-interrupt root control with a deterministic Pi extension command

**Failed invariant and evidence:**

- Task 6K's stronger model-facing prompt did not resolve the boundary. `/home/steven/.cache/kanedias/e2e/e2e-3569567-1786402419014244115/` proves root abort completed with exact `message_end(aborted) -> agent_end -> agent_settled`; a fresh generation then received the 198-byte prompt explicitly stating that the prior request was aborted, must not be resumed, no tools may be called, and only the marker may be returned.
- The local model again resumed the old twelve-file task, emitted an assistant tool-use message, at least seventeen tool-result messages, and had not produced `agent_end`/`agent_settled` when the observable four-minute settlement gate expired. Exact teardown restored baseline and the proxy log remained quiet.
- Two real local-model runs therefore establish a reproducible instruction-priority limitation after aborted history. This is not a missing prompt, stale event replay, supervisor disconnect, tool-call transport failure, or insufficient timeout. Retrying, strengthening the wording again, accepting tool output, or extending the wait would hide the failure rather than test supervisor lifecycle.
- The interrupt scenario's required post-abort invariant is that the same root Pi/supervisor control plane remains addressable and usable. Prove that deterministically with the installed `/present_e2e_question` extension command: exact prompt acknowledgement, root question projection, exact answer routing/removal, and idle/zero state. The scenario still uses the real local model for the active root task, root abort, active deep-child task, and deep-child abort.

**Files and scope:**

- Modify/Test only: `internal/supervisor/live_rpc_lifecycle_test.go`, `internal/supervisor/live_rpc_lifecycle_support_test.go`
- Reuse test-only extension fixture: `present_e2e_question` registered in `internal/image/pi-extension/src/index.ts`
- Do not modify the extension, Pi, provider/model, production routing, abort, timeouts, or generic non-interrupt root-usability probes.
- Remove the superseded Task 6K model-facing helper/test/wrapper rather than leaving dead acceptance code.

Independent review of this Task 6M amendment is required before implementation.

- [ ] **Step 1: Add RED pure prompt/question-selection regressions**

Add `TestLifecycleInterruptControlProbeRequiresExactQuestion`. Require missing pure helpers that:

1. construct exactly `/present_e2e_question MARKER`, retain the complete marker exactly once, and reject/avoid empty marker construction; and
2. select exactly one pending question for the target session whose title equals the supplied marker, returning its nonempty ID; reject no match, wrong session/title, empty ID, or duplicate exact matches.

Use in-memory `NodeSnapshot` trees only. Prove compile RED for the missing helpers before adding them.

- [ ] **Step 2: Replace post-interrupt model prompts with the controlled question probe**

Add `assertLifecycleRootControlAfterInterrupt(root, marker)` in the lifecycle support test:

1. Send exactly one `prompt` RPC containing the extension command from Step 1 through `lifecycleRPCCommand`; require its exact successful acknowledgement.
2. Poll the direct root `/v1/tree` until Step 1's selector finds exactly one matching pending root question. No sleep or action retry.
3. Answer it exactly once through the direct supervisor question-response route and require HTTP 204.
4. Poll until that exact question disappears.
5. Read typed `get_state` and require `IsStreaming == false` and `PendingMessageCount == 0`. Do not wait for or manufacture `agent_settled`; extension commands are handled without a model generation.

Use distinct exact run markers for the root-after-root-abort and root-after-child-abort probes. Replace both interrupt scenario calls to `assertRootUsable`/Task-6K wrapper with this deterministic probe. Remove `lifecyclePostAbortProbe`, `TestLifecyclePostAbortProbeExplicitlySupersedesPriorTask`, and the specialized model wrapper; preserve generic `assertRootUsable` byte-for-byte for every non-interrupt scenario.

Update the interrupt root's exact settlement total from the aborted root generation plus two model probes to only the aborted root generation. Keep the child aborted settlement total exact. The accepted extension prompt, projected question, answer, removal, and idle state replace—not waive—the two usability boundaries.

- [ ] **Step 3: Prove focused GREEN/race and tagged compilation**

```bash
gofmt -w internal/supervisor/live_rpc_lifecycle_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLifecycleInterruptControlProbeRequiresExactQuestion$'
go test -race -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLifecycleInterruptControlProbeRequiresExactQuestion$'
go test -count=1 -tags=incus ./internal/supervisor -run '^$'
git diff --check
```

Expected: exact command/selector cases pass normally and under race; no production file changes; tagged compilation and diff check pass.

- [ ] **Step 4: Rerun interrupt once on Task 6L**

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveRPCInterruptLifecycle$' -timeout 90m
```

Expected: deterministic root control passes after root abort; routed child abort response returns success before child terminal 409; deterministic root control passes again after child abort; exact root/child event/action/resource invariants and baseline restoration pass.

- [ ] **Step 5: Review, commit, and resume remaining live gates**

After independent implementation review:

```bash
git add internal/supervisor/live_rpc_lifecycle_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
git commit -m "test: use deterministic post-interrupt probe"
```

Then rerun Task 6H rapid control and Task 6I deterministic children on the final code, followed by all eight scenarios once. Any new failure receives another evidence-gated amendment.

---

### Task 6N: Bound deterministic-child reading context while proving real tool execution

**Failed invariant and evidence:**

- On final-code Task 6I verification, `/home/steven/.cache/kanedias/e2e/e2e-3618702-1786404767107214377/` proves direct single completed as a typed 200 read child with the concise exact marker. All three simultaneously observed parallel children then returned typed `child_failed` with `read child ended with stop reason error`.
- Each parallel child's Pi stream contains `agent_start`, many successful assistant `toolUse` rounds and tool-result messages, then `message_end(stopReason=error)` with the same exact internal error `Context size has been exceeded.` Two children emitted `agent_end`; teardown of the third began after all three terminal create calls had already returned the same typed error. Exact cleanup restored baseline; proxy remained quiet.
- The deterministic parallel task asks every child to read README, `go.mod`, and at least five Go source files. In this multi-repository workspace the local model repeatedly searches/reads broad content until its context overflows. This is a real local-model tool-workload failure, not concurrency admission, provider binding, marker transcription, supervisor transport, or timeout.
- The lifecycle acceptance needs one real read-tool execution per child, strict typed/result identity, simultaneous three-child topology, strict per-run marker, natural settlement, and cleanup. It does not need a context-expanding seven-file survey. Bound the task rather than retrying, increasing context, accepting an error, or removing tool evidence.

**Files and scope:**

- Modify/Test only: `internal/supervisor/live_rpc_lifecycle_test.go`, `internal/supervisor/live_rpc_lifecycle_support_test.go`
- Do not modify provider/model context, Pi, production supervisor, timeouts, parallelism, exact marker matching, or child count.

Independent review of this Task 6N amendment is required before implementation.

- [ ] **Step 1: Add RED bounded-task and read-tool evidence regressions**

Add pure helpers/tests:

1. `lifecycleDeterministicReadTask(marker)` returns a nonempty task containing the exact marker once, requires reading only `README.md`, asks for one concise repository-identification sentence, forbids modification/delegation/other-file inspection, and contains none of the prior multi-file/internal-path workload.
2. `validateLifecycleReadToolEvents(events, childIDs)` requires every nonempty distinct child ID to have at least one Pi `tool_execution_start` with `toolName == "read"`; rejects missing child evidence, duplicate/empty expected IDs, and any `delegate_session` tool start by those children. It ignores root and unrelated-session events and never inspects/retains tool arguments or output.

Use synthetic `EventEnvelope` values only. Prove compile RED for the missing helpers before implementation.

- [ ] **Step 2: Use the bounded task for direct single and all parallel children**

In `exerciseDeterministicChildren`, replace the direct-single and direct-parallel repository prompts with `lifecycleDeterministicReadTask(marker)`. Preserve:

- one single child followed by three concurrently started child calls;
- one snapshot containing all three parallel children before any call result is consumed;
- strict typed reviewer/read/session identity and distinct result IDs;
- concise exact markers from Task 6I;
- natural disappearance, process/socket/resource cleanup, root usability, and final invariants.

After each phase's child calls settle but before discarding their identities, apply `validateLifecycleReadToolEvents` to the durable journal for the exact single/parallel child IDs. Failure is an acceptance failure; do not infer tool use from model text.

- [ ] **Step 3: Prove focused GREEN/race and tagged compilation**

```bash
gofmt -w internal/supervisor/live_rpc_lifecycle_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLifecycleDeterministicReadTaskRequiresBoundedRealToolEvidence$'
go test -race -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLifecycleDeterministicReadTaskRequiresBoundedRealToolEvidence$'
go test -count=1 -tags=incus ./internal/supervisor -run '^$'
git diff --check
```

Expected: bounded prompt and strict per-child read-tool evidence cases pass normally/race; tagged compile and diff check pass; no production edit.

- [ ] **Step 4: Rerun deterministic children once**

```bash
set -a; . /home/steven/source/github/kanedias/.env; set +a
go test -v -count=1 -tags=incus ./internal/supervisor \
  -run '^TestLiveRPCDeterministicChildLifecycle$' -timeout 90m
```

Expected: exact single and three-parallel topology, at least one real read tool per child, strict concise markers and typed IDs, natural cleanup, reusable root, final event/action/resource invariants, and exact baseline restoration.

- [ ] **Step 5: Review, commit, and run the complete one-pass matrix**

```bash
git add internal/supervisor/live_rpc_lifecycle_test.go internal/supervisor/live_rpc_lifecycle_support_test.go
git commit -m "test: bound deterministic child workload"
```

After independent implementation review and live GREEN, rerun all eight scenarios once. Any new failure receives another evidence-gated amendment before Task 7.

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
