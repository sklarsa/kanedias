# Live Incus Exec and Readiness Fixes Design

## Goal

Fix the live Incus lifecycle failures revealed by image creation, workspace synchronization, and sandbox creation without changing command shape, profiles, configuration, or dependencies.

## Evidence and Root Causes

The Incus exec API separates operation completion, output-stream completion, and guest-command exit status. The current adapter waits only for the operation and therefore:

- treats a guest command with non-zero `metadata["return"]` as successful;
- can return before stdout/stderr websocket writers finish;
- causes workspace path probes such as `test -e` to take the success branch on exit status 1.

Incus v7.3.0's CLI provides the reference behavior: it supplies `InstanceExecArgs.DataDone`, waits for the operation, reads `op.Get().Metadata["return"]` as `float64`, and waits for `DataDone` so output is flushed.

A newly started instance can reject the first `systemctl is-system-running --wait` exec before the system bus exists. The current sandbox workflow makes one attempt, sees empty state plus the pre-bus error, and fails immediately. This is a transient readiness condition, not a terminal state.

Workspace synchronization currently waits for DNS and then runs `update-ca-certificates`, but DNS availability does not imply the guest has completed system initialization. This allowed the CA update to race missing `/tmp` startup setup.

## Approved Design

### Incus exec completion

`internal/incusclient.Exec` will create and pass a `DataDone` channel, wait for the exec operation with the request context, and wait for output completion before returning captured stdout and stderr. It will then inspect the completed operation's metadata.

The adapter will require `metadata["return"]` to exist and be a finite, integral numeric value representable as an `int`. Missing or malformed metadata returns a clear error while preserving captured output. A non-zero return status returns a clear exit-status error while preserving captured output. Request cancellation remains authoritative while either operation completion or output completion is pending.

The narrow private operation seam used by adapter tests will expose both `WaitContext(context.Context)` and `Get() api.Operation`, matching the relevant Incus operation contract.

### Condition-based systemd readiness

A focused readiness helper in `internal/sandbox` will repeatedly execute:

```text
systemctl is-system-running --wait
```

under one request-derived 60-second timeout. It succeeds only when trimmed stdout is `running` or `degraded`. A transient exec failure or another state causes another condition check. It stops immediately when the request is cancelled or the readiness deadline expires, returning an error with the last observed state/exec failure for diagnosis.

No fixed attempt count will be used. A small poll interval prevents a hot loop; tests inject the interval and timeout so they remain deterministic and fast.

### Workspace ordering

Workspace synchronization will use the same readiness semantics immediately after starting its temporary instance. Only after systemd reports `running` or `degraded` will it run `update-ca-certificates`. The existing bounded DNS wait remains and follows the CA update, preserving its 60-second limit before GitHub-dependent setup and repository operations.

Each workflow will keep a focused private readiness helper using its existing minimal exec interface. This avoids widening the public Incus client API or introducing a new cross-workflow abstraction; tests pin the same accepted states and bounds in both workflows.

## Error and Cancellation Behavior

- Exec always returns any stdout/stderr captured before an operation, metadata, exit-status, flush, or cancellation error.
- Missing/malformed return metadata is a protocol error, not success.
- Non-zero guest exit status is an error, allowing workflow probes to distinguish absence correctly.
- Readiness retry loops are bounded by the existing 60-second timeout and stop on request cancellation.
- Existing cleanup continues under `context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)`.

## Scope Boundaries

This change does not alter Cobra commands, configuration, profiles, storage naming, DNS timeout, cleanup timeout, dependencies, or repository synchronization behavior beyond correcting exec status handling and readiness ordering. Production code continues to use only the Incus Go client and never invokes the `incus` executable.

## Verification Strategy

TDD will cover:

1. adapter non-zero return status, missing/malformed metadata, output flush, and output preservation;
2. systemd pre-bus failure followed by `running` and `degraded`, plus timeout and cancellation;
3. workspace ordering: readiness before CA update, CA update before bounded DNS, and no downstream work before readiness;
4. all existing package and repository tests, race tests, vet, build, formatting, diff checks, and a production source scan for Incus CLI invocation;
5. bounded live reruns with the ignored absolute config path, followed by a temporary Go-client-only daemon cleanup probe.

## Self-Review

- The design addresses each observed failure at its source: exec status/flush, transient systemd startup, and workspace readiness ordering.
- Cancellation and the existing 60-second/30-second bounds remain explicit.
- Tests can force every new branch without a live daemon.
- No CLI, config, profile, dependency, or unrelated architecture change is introduced.
- Readiness stays inside the existing workflow packages, avoiding a new abstraction while tests pin equivalent semantics.
