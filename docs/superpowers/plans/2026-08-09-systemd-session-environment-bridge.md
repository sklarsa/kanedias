# Systemd Session Environment Bridge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ensure socket-activated Pi sessions receive the proxy, GitHub placeholder, and Kanedias supervision variables already configured on each Incus instance.

**Architecture:** Keep Incus `environment.*` as the source of truth because it correctly populates container PID 1 and `incus exec`. Add a root-only `ExecStartPre` bridge that copies only an explicit allowlist from `/proc/1/environ` into an atomically replaced systemd `EnvironmentFile`; the main Pi process continues to run as the unprivileged `kanedias` user. Make the Pi launcher fail at startup if supervised session identity is absent so this cannot silently degrade again.

**Tech Stack:** Go 1.24, Bash, systemd socket activation, Incus containers, Go tests.

## Global Constraints

- Work only in `/home/steven/source/github/kanedias/.worktrees/fix-proxy-subagents` on `fix/proxy-subagents`.
- Preserve `User=kanedias` and `Group=kanedias` for the Pi process; only the fixed image-owned environment bridge runs with systemd's `+` privilege prefix.
- Export only explicitly named runtime variables; never copy the complete PID 1 environment.
- Do not expose the real host GitHub credential. The guest receives only `GH_TOKEN=container-dummy`; the host proxy continues to inject the real credential.
- Keep `environment.*` configuration for `incus exec` and existing lifecycle behavior.

---

### Task 1: Bridge Incus PID 1 Environment into the Pi Service

**Files:**
- Create: `internal/image/kanedias-pi-env`
- Modify: `internal/image/kanedias-pi@.service`
- Modify: `internal/image/kanedias-pi-rpc`
- Modify: `internal/image/image.go`
- Modify: `internal/image/install.sh`
- Test: `internal/image/image_test.go`

**Interfaces:**
- Consumes: NUL-delimited PID 1 environment at `/proc/1/environ` and the root-owned `/run/kanedias-pi` runtime directory.
- Produces: `/run/kanedias-pi/pi.env`, a systemd-compatible allowlisted environment file loaded before `/usr/local/libexec/kanedias-pi-rpc` starts.

- [ ] **Step 1: Write failing tests for the environment bridge and image wiring**

Add a test that executes the embedded bridge against a temporary NUL-delimited environment containing all required proxy/session values plus a forbidden sentinel. Assert the output includes correctly quoted allowlisted assignments, excludes the sentinel, and is mode `0600`. Add a missing-required-value case that fails without replacing the destination. Extend the image workflow assertions to require upload/install of `kanedias-pi-env` and these service directives:

```ini
EnvironmentFile=-/run/kanedias-pi/pi.env
ExecStartPre=+/usr/bin/env -i /usr/bin/bash --noprofile --norc /usr/local/libexec/kanedias-pi-env
```

Also retain:

```ini
User=kanedias
Group=kanedias
```

- [ ] **Step 2: Run focused tests and verify RED**

Run:

```bash
go test ./internal/image -run 'TestPiEnvironmentBridge|TestCreateRunsImageWorkflowInOrder' -count=1
```

Expected: FAIL because `kanedias-pi-env` is not embedded/installed and the service lacks the bridge directives.

- [ ] **Step 3: Implement the minimal bridge**

Create `internal/image/kanedias-pi-env` as an image-owned Bash script. It must:

```bash
source_path=${1:-/proc/1/environ}
destination=${2:-/run/kanedias-pi/pi.env}
```

Read NUL-delimited entries, copy only the exact proxy/CA/GitHub/Kanedias allowlist, reject newline-bearing values, double-quote and escape systemd environment-file values, require `KANEDIAS_SESSION_ID`, `KANEDIAS_SESSION_KIND`, `KANEDIAS_SUPERVISOR_SOCKET`, `HTTP_PROXY`, `HTTPS_PROXY`, `GH_TOKEN`, `SSL_CERT_FILE`, and `NODE_EXTRA_CA_CERTS`, then atomically replace the destination at mode `0600`.

Embed/upload/install it in `internal/image/image.go` and `internal/image/install.sh`. Configure `internal/image/kanedias-pi@.service` with optional `EnvironmentFile` plus privileged `ExecStartPre`, while leaving the main service user/group unchanged.

- [ ] **Step 4: Make the launcher fail closed**

In `internal/image/kanedias-pi-rpc`, require both identity fields before selecting root/child arguments:

```bash
: "${KANEDIAS_SESSION_ID:?KANEDIAS_SESSION_ID is required}"
: "${KANEDIAS_SESSION_KIND:?KANEDIAS_SESSION_KIND is required}"
```

Update launcher tests so valid root/child cases include an ID, and add a missing-ID test proving Pi is never invoked.

- [ ] **Step 5: Run focused tests and verify GREEN**

Run:

```bash
go test ./internal/image -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/image/kanedias-pi-env internal/image/kanedias-pi@.service \
  internal/image/kanedias-pi-rpc internal/image/image.go internal/image/install.sh \
  internal/image/image_test.go docs/superpowers/plans/2026-08-09-systemd-session-environment-bridge.md
git commit -m "fix: propagate session environment to Pi service"
```

### Task 2: Validate Both User-Visible Failures

**Files:**
- Modify only if validation finds a test gap: `internal/profiles/profiles_test.go`
- Test: repository-wide Go suite and image shell checks

**Interfaces:**
- Consumes: the bridge from Task 1 and existing sandbox profile values.
- Produces: evidence that Pi receives `KANEDIAS_SESSION_ID`, proxy variables, and the dummy GitHub token without changing host credential injection.

- [ ] **Step 1: Add a profile assertion only if not already covered**

Ensure a test checks the rendered sandbox profile contains:

```yaml
environment.GH_TOKEN: "container-dummy"
environment.HTTP_PROXY: "http://10.76.111.1:3128"
environment.HTTPS_PROXY: "http://10.76.111.1:3128"
```

If existing assertions already prove all three, do not add a redundant test.

- [ ] **Step 2: Run shell/static validation**

Run:

```bash
bash -n internal/image/kanedias-pi-env internal/image/kanedias-pi-rpc
```

Expected: exit 0.

- [ ] **Step 3: Run the full hermetic suite**

Run:

```bash
go test ./... -count=1
```

Expected: all packages PASS.

- [ ] **Step 4: Run an isolated systemd/Incus mechanism probe**

In a disposable container from the current sandbox image, use a temporary unit with:

```ini
User=kanedias
EnvironmentFile=-/run/kanedias/pi.env
ExecStartPre=+/usr/local/libexec/probe-env
```

Verify the pre-start helper runs as UID 0, the main process runs as UID 1000, and an `environment.KANEDIAS_SESSION_ID` value from PID 1 reaches the main service environment. Delete the disposable container afterward.

- [ ] **Step 5: Review the final diff and repository status**

Run:

```bash
git diff --check
git status --short
git diff --stat main...HEAD
git diff main...HEAD -- internal/image docs/superpowers/plans
```

Expected: no whitespace errors, only intended files changed, and no staged/untracked artifacts beyond the plan and implementation.

- [ ] **Step 6: Commit any Task 2-only test adjustment**

If Task 2 changed a test file:

```bash
git add internal/profiles/profiles_test.go
git commit -m "test: cover sandbox GitHub proxy environment"
```

Otherwise no second commit is needed.
