# RPC Lifecycle Hardening Design

**Date:** 2026-08-10

**Status:** Approved for planning

## Summary

Kanedias has live Incus acceptance coverage for isolated lifecycle operations,
but it does not repeatedly exercise the complete local-model control matrix. In
particular, it does not prove that a local model can reliably create multiple
simultaneous descendants or that mixed stop, interrupt, steer, natural child
completion, and root teardown remain correct over repeated runs.

This campaign adds a focused live lifecycle suite using the configured
`local-executor` provider and `Qwen3.6-27B-GGUF` model through the real Pi RPC,
supervisor, descendant SSE, egress proxy, Incus, and server control paths. It
combines deterministic supervisor actions with simple model-directed
`delegate_session` prompts so failures can be localized without excusing tool
call failures that the local model should be able to handle.

After each reproduced infrastructure defect, the fix must be test-driven from a
minimal deterministic regression. The final stability bar is five consecutive
clean runs of every lifecycle scenario after the last code change.

## Goals

- Prove that one local-model-directed descendant can spawn, settle naturally,
  return its result, and leave the root controllable.
- Prove that multiple descendants can be active simultaneously, settle
  independently, and return distinct results without losing lifecycle events.
- Prove child stop, root stop, interrupt, and steer behavior while model work is
  active.
- Prove mixed parallel outcomes: natural completion, explicit stop, and
  interrupt in sibling descendants.
- Exercise both direct supervisor APIs and model-issued `delegate_session`
  calls through the same production paths.
- Preserve enough evidence to identify whether a failure begins in the model
  provider exchange, Pi RPC, supervisor state machine, descendant transport,
  proxy, or cleanup path.
- Require exact cleanup of session processes, sockets, Incus instances,
  workspace volumes, subscriptions, and expected proxy teardown.
- Convert every confirmed code defect into an automated regression before
  changing implementation code.

## Non-goals

- Judging broad reasoning quality or answer prose from the local model.
- Supporting arbitrary prompts as lifecycle acceptance fixtures.
- Adding a second subagent implementation inside the Incus image. The existing
  `delegate_session` extension remains the model-facing child seam.
- Replacing the existing recursive and server-managed live acceptance suites.
- Stressing paid providers, benchmarking model throughput, or comparing model
  quality.
- Treating nondeterministic retries as a fix for a reproducible transport or
  state-machine defect.

## Design Principles

### Hybrid execution

Every control family begins with a deterministic operation against the real
supervisor API. This removes model choice from the reproduction while retaining
the real Pi process, proxy, Incus resources, RPC transport, and child process.
Key spawn and settlement scenarios are then repeated through short, exact
local-model prompts that require `delegate_session`.

The deterministic layer is diagnostic, not a substitute for the model layer. A
simple model-directed tool call that fails is a campaign failure. Investigation
must capture the provider request and response, exposed tool schema, Pi events,
and supervisor state before deciding whether the defect is model behavior or
Kanedias integration. Proven model noncompliance may be addressed by making the
fixture more explicit, but must not conceal an RPC, tool exposure, lifecycle, or
cleanup failure.

### Fresh isolation

Each scenario starts an owned proxy and a fresh root unless the scenario
explicitly tests repeated operations on one root. A scenario records its exact
starting resource baseline and must return to it. No scenario may depend on a
session or artifact created by a preceding scenario.

### Observable terminal outcomes

Every accepted lifecycle action must reach one documented terminal outcome.
The harness must not infer success from an HTTP acceptance response alone. It
waits for the corresponding Pi event or supervisor transition, confirms the
remaining tree is usable, and verifies resource cleanup.

## Test Harness Structure

Add a focused opt-in live test file beside `internal/supervisor/live_incus_test.go`.
It reuses the existing `liveAcceptance` provisioning, process, SSE, RPC, tree,
resource, and cleanup helpers. Lifecycle-specific orchestration and assertions
remain in the new file so the already-large general acceptance file does not
accumulate another scenario matrix.

The lifecycle harness provides four focused capabilities:

1. **Scenario setup:** build the reviewed checkout, start the owned proxy, start
   a fresh root, subscribe to events, and record the baseline.
2. **Action driver:** issue direct child requests concurrently or send exact
   local-model prompts and RPC controls.
3. **Evidence recorder:** retain ordered events, action requests and responses,
   tree snapshots, provider/Pi/supervisor/proxy logs, process state, and Incus
   resources under the existing failure artifact directory.
4. **Invariant checker:** wait for terminal events, probe surviving sessions,
   verify tree state, inspect teardown logs, and compare exact resources with
   the baseline.

Successful runs remove transient artifacts through the existing harness
cleanup. Failed runs preserve them and print their location.

## Scenario Matrix

### 1. Single model-directed child, natural completion

Prompt the root to call `delegate_session` exactly once with a short read-only
task containing a unique marker. Require one visible child, distinct root and
Pi identities, the correct worker/model metadata, a successful terminal child
result, the marker in the root's final response, child disappearance, and a
successful later root `get_state` call.

### 2. Multiple model-directed children, parallel completion

Prompt the root to issue three independent `delegate_session` calls for three
unique markers in the same turn and then aggregate all results. Require all
three children to be visible simultaneously before any are allowed to finish,
unique identities and resources, independent terminal settlements, all markers
in the final root response, and no missing or duplicate lifecycle events.

A deterministic companion starts the same three child requests concurrently
through `/v1/sessions/{id}/children`. It proves whether parallel child support
works independently of the model's emitted tool calls.

### 3. Stop one active child

Create a child held at a controlled active boundary, stop it through the public
session control route, and require the pending child request to settle with the
canonical stopped/canceled outcome. The sibling-free root must return to
`ready`, accept a new RPC prompt, and retain no child resources or socket.

### 4. Gracefully end a root with active children

Start three controlled children and stop the root through the public session
stop route. Require the root process to exit, all descendants to terminate,
all sockets to disappear, and exact Incus instance and volume cleanup. This is
the lifecycle definition of ending the long-lived interactive root session;
natural child completion remains covered separately.

### 5. Interrupt active generation

Start a deterministic long-running generation, wait for evidence that the
target is running, then issue interrupt. Require the interrupted turn to settle
without closing Pi RPC, duplicating terminal events, or changing unrelated
sessions. A subsequent prompt on the surviving session must complete.

Repeat the same flow against a model-created child to cover routed descendant
control.

### 6. Steer active generation

Start a generation whose original response cannot already contain a unique
steer marker, wait until it is running, then steer it with that marker. Require
successful control acknowledgement, one coherent terminal turn containing the
marker, an open RPC transport, and a usable subsequent turn.

### 7. Rapid steer, interrupt, and follow-up

While a target is active, issue steer followed by interrupt, wait for the
interrupted turn to settle, then send a normal follow-up prompt. Require each
accepted command to have one outcome, no stale steer to leak into the follow-up,
and no deadlock or transport closure.

### 8. Mixed sibling outcomes

Create three simultaneous children. Allow one to complete naturally, stop the
second, and interrupt the third before allowing it to finish. Require the
natural result to remain intact, canonical terminal outcomes for the controlled
siblings, independent cleanup, and a ready, controllable root after all three
disappear.

## Invariants

The following apply to every scenario:

- Accepted events remain ordered and are not silently lost or duplicated.
- Each session has one stable session ID, Pi session ID, session file, process,
  socket, instance, and volume identity while live.
- A terminal child disappears from the tree only after its terminal result has
  been delivered or its canonical control error has settled the caller.
- Stopping or interrupting one child does not terminate a sibling or root.
- A surviving root remains RPC-controllable after every child outcome.
- A stopped root rejects later control cleanly and leaves no owned process,
  socket, instance, or volume.
- Genuine event overflow remains fatal under the existing 4,096-event or
  16-MiB limits; the lifecycle suite must not weaken those bounds.
- Expected EOF, cancellation, reset, broken-pipe, and closed-connection proxy
  teardown emits no warning. Unexpected proxy diagnostics remain classified
  without hosts, paths, credentials, or raw errors.

## Evidence and Failure Classification

For each action, record a monotonic timestamp, scenario name, iteration,
session ID, target lifecycle before and after, request type, normalized response,
and related event sequence range. Capture tree and Incus snapshots at setup,
pre-control, post-control, and teardown boundaries.

Failures are investigated from the first divergent boundary:

1. **Provider/tool emission:** request schema, model response, and Pi tool-call
   events.
2. **Pi RPC:** command acknowledgement, event ordering, settlement, and
   transport status.
3. **Supervisor:** lifecycle transition, routing, child result, and tree state.
4. **Descendant stream:** subscription completion, terminal event propagation,
   and mirror state.
5. **Proxy:** expected versus unexpected privacy-safe diagnostics.
6. **Cleanup:** processes, sockets, instances, volumes, and retained metadata.

The classification describes the observed boundary; it does not waive the
scenario. In particular, failure of the local model to perform the simple
required tool call still fails model-directed acceptance and must be understood.

## Debugging and Fix Loop

For each failure:

1. Preserve the full failed live-run artifacts.
2. Identify the earliest invariant violation rather than patching the final
   symptom.
3. Reproduce that violation in the narrowest deterministic unit or integration
   test that still exercises the defective boundary.
4. Confirm the regression test fails for the expected reason.
5. Implement the smallest root-cause fix.
6. Run the focused test, its package race tests, and the failed live scenario.
7. Reset that scenario's consecutive-success count after any code change.

Do not add blind sleeps or retries to turn races into apparent success. Polling
may wait only for an observable state with a bounded deadline.

## Stability and Verification

The final live gate runs every scenario five consecutive times after the last
implementation change. A scenario's count resets to zero after any failure or
code change. The campaign is complete only when all scenarios reach five and
the Incus resource baseline is clean after each run.

Use repeated Go test execution rather than hiding retries inside assertions, so
each iteration has an independent setup, teardown, result, and artifact set.
After the live matrix passes, run:

```bash
make test
go test -race ./internal/supervisor/... ./internal/supervisorapi ./internal/manager ./internal/proxy
make build
make lint
git diff --check
```

Also inspect the final proxy and supervisor logs for teardown warnings and
confirm that no test-owned Incus instance, volume, Unix socket, or process
remains.

## Operational Outcome

A simple local model can reliably create one or several supervised descendants,
and operators can stop, interrupt, steer, or end sessions without losing
settlement, wedging RPC, affecting siblings, or leaking resources. When a
failure occurs, retained evidence identifies the first broken boundary rather
than presenting only a downstream disconnect or timeout.
