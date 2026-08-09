# Configurable Server Listen Address Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the Makefile's web-server bind address and port configurable, default both server targets to `0.0.0.0:8080`, merge the change through a pull request, update local `main`, and launch the service.

**Architecture:** The Makefile will expose overridable `BIND` and `PORT` variables and derive one `LISTEN` value consumed by both server-starting targets. Validation will use Make dry runs for default and overridden arguments, followed by the existing test suite; deployment will use the merged `main` checkout.

**Tech Stack:** GNU Make, Go CLI, Git, GitHub CLI, POSIX shell

## Global Constraints

- `BIND` defaults to `0.0.0.0`.
- `PORT` defaults to `8080`.
- Both `run` and `server` pass `--listen $(BIND):$(PORT)` through a shared `LISTEN` variable.
- The egress proxy address and behavior remain unchanged.
- Binding to all interfaces intentionally exposes the web server subject to host firewall and network policy.

## File Structure

- Modify `Makefile`: define the listen variables, use them in `run` and `server`, and update target help text.
- No production source files or dependencies change.

---

### Task 1: Configure Makefile Server Listen Address

**Files:**
- Modify: `Makefile:1-42`
- Test: Make dry-run assertions and the existing repository test suite

**Interfaces:**
- Consumes: GNU Make command-line variable overrides such as `BIND=127.0.0.1 PORT=9000`.
- Produces: `LISTEN := $(BIND):$(PORT)`, passed to the existing CLI interface `server --listen <address>`.

- [ ] **Step 1: Run default-listen assertions and verify they fail before the change**

```bash
make -n server | grep -F -- 'server --listen 0.0.0.0:8080'
make -n run | grep -F -- 'server --listen 0.0.0.0:8080'
```

Expected: both commands exit non-zero because the targets still contain `127.0.0.1:8080`.

- [ ] **Step 2: Run override assertions and verify they fail before the change**

```bash
make -n server BIND=127.0.0.1 PORT=9000 | grep -F -- 'server --listen 127.0.0.1:9000'
make -n run BIND=192.0.2.10 PORT=9090 | grep -F -- 'server --listen 192.0.2.10:9090'
```

Expected: both commands exit non-zero because `BIND` and `PORT` are not yet consumed.

- [ ] **Step 3: Add the minimal Makefile implementation**

Add these definitions after `CONFIG ?= config.toml`:

```make
# Web server listen address. Override with, for example,
# `make run BIND=127.0.0.1 PORT=9000`.
BIND ?= 0.0.0.0
PORT ?= 8080
LISTEN := $(BIND):$(PORT)
```

Change the two target declarations and server commands to:

```make
run: build ## Run the egress proxy (only if not already up) + web server on BIND:PORT (defaults to 0.0.0.0:8080); Ctrl-C stops the server (and the proxy only if we started it)
```

```make
	$(BINARY) --config $(CONFIG) server --listen $(LISTEN)
```

```make
server: build ## Run the web server on BIND:PORT (defaults to 0.0.0.0:8080; sessions also need the proxy; see `proxy` or `run`)
	$(BINARY) --config $(CONFIG) server --listen $(LISTEN)
```

Do not change the proxy target or proxy address.

- [ ] **Step 4: Verify default and overridden generated commands**

```bash
make -n server | grep -F -- 'server --listen 0.0.0.0:8080'
make -n run | grep -F -- 'server --listen 0.0.0.0:8080'
make -n server BIND=127.0.0.1 PORT=9000 | grep -F -- 'server --listen 127.0.0.1:9000'
make -n run BIND=192.0.2.10 PORT=9090 | grep -F -- 'server --listen 192.0.2.10:9090'
make help | grep -F -- 'defaults to 0.0.0.0:8080'
```

Expected: every command exits zero and prints the matching target command or help text.

- [ ] **Step 5: Run the existing test suite**

```bash
make test
```

Expected: Go and Node test suites pass.

- [ ] **Step 6: Commit the implementation**

```bash
git add Makefile
git commit -m "feat: configure server bind address and port"
```

Expected: one implementation commit containing only the Makefile change.

---

### Task 2: Deliver and Launch the Change

**Files:**
- No file changes

**Interfaces:**
- Consumes: the tested feature branch and GitHub repository permissions.
- Produces: a merged pull request, an up-to-date local `main`, and a server listening on `0.0.0.0:8080`.

- [ ] **Step 1: Push the feature branch and open a pull request**

From the feature worktree:

```bash
git push -u origin feat/configurable-server-listen
gh pr create \
  --base main \
  --head feat/configurable-server-listen \
  --title "feat: configure server bind address and port" \
  --body $'## Summary\n- add overridable BIND and PORT Makefile variables\n- default run and server to 0.0.0.0:8080\n- document the network exposure behavior\n\n## Validation\n- make dry-run assertions\n- make test'
```

Expected: GitHub reports the new pull-request URL.

- [ ] **Step 2: Inspect checks and merge while preserving commits**

```bash
pr_number="$(gh pr view --json number --jq .number)"
gh pr checks "$pr_number" || true
gh pr merge "$pr_number" --merge --delete-branch
```

Expected: the pull request is merged. A merge commit is used so the existing local documentation commits remain ancestors of remote `main` and local `main` can fast-forward.

- [ ] **Step 3: Update the original `main` checkout**

From the original repository checkout:

```bash
git switch main
git pull --ff-only origin main
git status --short --branch
```

Expected: `main` fast-forwards to `origin/main` and the working tree is clean.

- [ ] **Step 4: Start the merged service detached on all IPv4 interfaces**

First inspect port 8080 so an unrelated process is not terminated:

```bash
ss -ltnp '( sport = :8080 )' || true
```

If no process is listening, launch the service from the original `main` checkout:

```bash
mkdir -p .run
nohup make run BIND=0.0.0.0 PORT=8080 >.run/kanedias.log 2>&1 &
echo $! >.run/kanedias.pid
```

If a Kanedias process is already listening only on `127.0.0.1:8080`, stop that known process before running the launch commands. Do not terminate an unrelated process; report the conflict instead.

- [ ] **Step 5: Verify network binding and local HTTP reachability**

```bash
for attempt in 1 2 3 4 5; do
  if ss -ltn '( sport = :8080 )' | grep -Fq '0.0.0.0:8080'; then
    break
  fi
  sleep 1
done
ss -ltn '( sport = :8080 )' | grep -F -- '0.0.0.0:8080'
curl --fail --show-error --silent http://127.0.0.1:8080/ >/dev/null
```

Expected: the socket is bound to `0.0.0.0:8080` and the HTTP request succeeds. Report `.run/kanedias.log` and `.run/kanedias.pid` for operation and shutdown.
