# Incus-in-Incus Profile Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add and live-verify a separate additive profile that lets an unprivileged Incus system container run nested Incus containers.

**Architecture:** First prove the minimum outer-container settings on the local Incus 7.3 server using disposable Debian containers and directory-backed nested storage. Then add one embedded `incus-in-incus` profile to the existing profile registry and validate the rendered profile from a fresh outer container without per-instance overrides.

**Tech Stack:** Go 1.26, embedded YAML, Cobra, Incus 7.3, Debian 13 system containers, systemd

## Global Constraints

- The outer container remains unprivileged (`security.privileged: "false"`).
- The profile is additive, has `devices: {}`, and is named `incus-in-incus`.
- Support nested system containers only; nested virtual machines are out of scope.
- Do not modify `image-build`, `sandbox`, `default`, or any existing host profile.
- Do not add raw LXC/AppArmor overrides.
- Add only settings proven necessary by the live test.
- Use unique disposable names and remove all temporary host-side instances and profiles after each live test.

---

### Task 1: Prove the Minimum Unprivileged Nesting Configuration

**Files:**
- Temporary host-side Incus instance only: `kanedias-incus-probe-<timestamp>`

**Interfaces:**
- Consumes: local Incus 7.3 server, `images:debian/13` container image, Debian `incus` package.
- Produces: the exact configuration keys to encode in `incus-in-incus.yaml`.

- [ ] **Step 1: Establish the clean baseline**

Run:

```bash
git status --short --branch
go test -count=1 ./internal/profiles
incus version
incus list --format csv -c n
```

Expected: the worktree contains only the committed design and plan history, profile tests pass, client/server are 7.3, and no probe-named instance exists.

- [ ] **Step 2: Launch a bare disposable outer container**

Choose a collision-resistant name and launch only with `default`:

```bash
outer="kanedias-incus-probe-$(date -u +%Y%m%d%H%M%S)"
incus launch images:debian/13 "$outer" --profile default
```

Record `$outer` for cleanup. Do not alter `default` or any existing profile.

- [ ] **Step 3: Install Incus inside the bare outer container**

Run:

```bash
incus exec "$outer" -- bash -lc '
  set -eux
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y incus
  systemctl enable --now incus
  incus admin waitready --timeout=60
  incus version
'
```

Expected: the nested client and daemon are installed. If the daemon cannot become ready before nesting is enabled, retain the exact service error, continue with Step 4, and retry after the outer restart.

- [ ] **Step 4: Enable only documented unprivileged nesting**

Run:

```bash
incus stop "$outer"
incus config set "$outer" security.nesting=true
incus config set "$outer" security.privileged=false
incus start "$outer"
incus exec "$outer" -- bash -lc '
  set -eux
  systemctl enable --now incus
  incus admin waitready --timeout=60
'
```

Assert:

```bash
[[ $(incus config get "$outer" security.nesting) == true ]]
[[ $(incus config get "$outer" security.privileged) == false ]]
```

- [ ] **Step 5: Initialize directory-backed nested Incus**

Feed this exact preseed inside the outer container:

```bash
incus exec "$outer" -- bash -lc "cat <<'EOF' | incus admin init --preseed
config: {}
networks: []
storage_pools:
- name: default
  driver: dir
profiles:
- name: default
  config: {}
  description: Nested container profile
  devices:
    root:
      path: /
      pool: default
      type: disk
projects: []
cluster: null
EOF"
```

Expected: nested Incus has a `dir` pool and a root-only default profile, avoiding nested bridge and advanced-storage requirements.

- [ ] **Step 6: Launch and execute in an inner container**

Run:

```bash
incus exec "$outer" -- bash -lc '
  set -eux
  incus launch images:debian/13 inner
  incus exec inner -- sh -c "printf nested-incus-ok"
' | tee /tmp/kanedias-incus-probe-output

grep -Fq nested-incus-ok /tmp/kanedias-incus-probe-output
```

Expected: the inner system container reaches `RUNNING` and prints `nested-incus-ok`. If this fails, preserve `incus info inner --show-log`, `journalctl -u incus --no-pager`, and the outer instance log; diagnose the failing operation before testing one additional non-privileged setting. Do not enable privileged mode or raw overrides. Update this plan and the design if a setting beyond `security.nesting` is proven necessary.

- [ ] **Step 7: Remove the disposable probe and verify cleanup**

Run even after a failure:

```bash
incus delete "$outer" --force
rm -f /tmp/kanedias-incus-probe-output
! incus list "name=$outer" --format csv -c n | grep -Fxq "$outer"
```

Expected: the outer instance and all nested resources are gone.

---

### Task 2: Add the Embedded Additive Profile with TDD

**Files:**
- Create: `internal/profiles/incus-in-incus.yaml`
- Modify: `internal/profiles/profiles.go`
- Modify: `internal/profiles/profiles_test.go`

**Interfaces:**
- Produces: `const IncusInIncus Type = "incus-in-incus"`.
- Extends: `Types() []string` and `Render(io.Writer, string, config.Config) error`.
- Consumes: the minimum settings proven by Task 1.

- [ ] **Step 1: Add failing profile registration and rendering tests**

Extend `TestRenderProfiles` with:

```go
{name: "incus-in-incus", description: "Unprivileged container for running nested Incus containers"},
```

Add this focused test:

```go
func TestRenderIncusInIncusIsAdditiveAndUnprivileged(t *testing.T) {
    var output bytes.Buffer
    if err := Render(&output, "incus-in-incus", config.Config{}); err != nil {
        t.Fatal(err)
    }

    rendered := output.String()
    for _, want := range []string{
        "  security.nesting: \"true\"",
        "  security.privileged: \"false\"",
        "devices: {}",
    } {
        if !strings.Contains(rendered, want) {
            t.Errorf("rendered incus-in-incus profile missing %q", want)
        }
    }
}
```

Update `TestTypes` to expect:

```go
[]string{"image-build", "incus-in-incus", "lemonade", "sandbox"}
```

Update `TestRenderUnknownType` so its expected-name loop includes `incus-in-incus`.

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
go test -count=1 ./internal/profiles
```

Expected: FAIL because `incus-in-incus` is not registered or embedded.

- [ ] **Step 3: Add the minimal profile YAML**

Create `internal/profiles/incus-in-incus.yaml` with the settings proven by Task 1. If Task 1 succeeds with nesting alone, use exactly:

```yaml
config:
  security.nesting: "true"
  security.privileged: "false"
description: Unprivileged container for running nested Incus containers
devices: {}
```

- [ ] **Step 4: Register the profile**

Add the constant:

```go
IncusInIncus Type = "incus-in-incus"
```

Add the path entry:

```go
string(IncusInIncus): "incus-in-incus.yaml",
```

Return a new lexical-order slice from `Types()`:

```go
return []string{string(ImageBuild), string(IncusInIncus), string(Lemonade), string(Sandbox)}
```

No Cobra change is required because `cmd/profile.go` consumes `profiles.Types()`.

- [ ] **Step 5: Format and verify GREEN**

Run:

```bash
gofmt -w internal/profiles/profiles.go internal/profiles/profiles_test.go
go test -count=1 ./internal/profiles
go test -count=1 ./cmd
git diff --check
```

Expected: every command passes and the rendered profile ends with a newline.

- [ ] **Step 6: Commit the profile implementation**

Run:

```bash
git add internal/profiles/incus-in-incus.yaml internal/profiles/profiles.go internal/profiles/profiles_test.go
git commit -m "feat: add Incus-in-Incus profile"
```

Expected: one focused implementation commit.

---

### Task 3: Validate the Rendered Profile from a Fresh Container

**Files:**
- Temporary host-side profile: `kanedias-incus-profile-<timestamp>`
- Temporary host-side instance: `kanedias-incus-live-<timestamp>`

**Interfaces:**
- Consumes: `go run . --config ./config.toml profile incus-in-incus`.
- Produces: live proof that `default + incus-in-incus` works without per-instance overrides.

- [ ] **Step 1: Create a disposable host profile from the renderer**

Run:

```bash
stamp=$(date -u +%Y%m%d%H%M%S)
profile="kanedias-incus-profile-$stamp"
outer="kanedias-incus-live-$stamp"
incus profile create "$profile"
go run . --config ./config.toml profile incus-in-incus | incus profile edit "$profile"
incus profile show "$profile"
```

Expected: the temporary profile has no devices and contains only the proven configuration.

- [ ] **Step 2: Launch a fresh outer container using the rendered profile**

Run:

```bash
incus launch images:debian/13 "$outer" --profile default --profile "$profile"
```

Do not apply any per-instance configuration key.

- [ ] **Step 3: Install and initialize nested Incus**

Run:

```bash
incus exec "$outer" -- bash -lc "
  set -eux
  export DEBIAN_FRONTEND=noninteractive
  apt-get update
  apt-get install -y incus
  systemctl enable --now incus
  incus admin waitready --timeout=60
  cat <<'EOF' | incus admin init --preseed
config: {}
networks: []
storage_pools:
- name: default
  driver: dir
profiles:
- name: default
  config: {}
  description: Nested container profile
  devices:
    root:
      path: /
      pool: default
      type: disk
projects: []
cluster: null
EOF
"
```

Expected: the nested server becomes ready and initializes successfully.

- [ ] **Step 4: Prove the inner container works**

Run:

```bash
incus exec "$outer" -- bash -lc '
  set -eux
  incus launch images:debian/13 inner
  test "$(incus list inner --format csv -c s)" = RUNNING
  test "$(incus exec inner -- sh -c "printf nested-incus-profile-ok")" = nested-incus-profile-ok
'
```

Expected: all assertions pass.

- [ ] **Step 5: Remove temporary resources and prove cleanup**

Run even after a failure:

```bash
incus delete "$outer" --force
incus profile delete "$profile"
! incus list "name=$outer" --format csv -c n | grep -Fxq "$outer"
! incus profile list --format csv -c n | grep -Fxq "$profile"
```

Expected: neither temporary resource remains.

---

### Task 4: Complete Verification and Review

**Files:**
- No new files expected.

**Interfaces:**
- Verifies: all repository behavior and final Git state.

- [ ] **Step 1: Run complete non-live verification**

Run:

```bash
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build ./...
git diff --check
```

Expected: every command exits zero.

- [ ] **Step 2: Verify renderer output directly**

Run:

```bash
rendered=$(go run . --config ./config.toml profile incus-in-incus)
printf '%s\n' "$rendered" | grep -Fx '  security.nesting: "true"'
printf '%s\n' "$rendered" | grep -Fx '  security.privileged: "false"'
printf '%s\n' "$rendered" | grep -Fx 'devices: {}'
```

Expected: all exact lines are present.

- [ ] **Step 3: Request independent code review**

Review the implementation commit against `docs/superpowers/specs/2026-08-06-incus-in-incus-profile-design.md`. Require findings ordered by severity and focused on profile registration, additive behavior, privilege, unsupported extra settings, tests, and live-cleanup evidence. Apply only technically validated findings.

- [ ] **Step 4: Run final cleanliness checks**

Run:

```bash
git status --short --branch
git diff --check
incus list --format csv -c n | grep -E '^kanedias-incus-(probe|live)-' && exit 1 || true
incus profile list --format csv -c n | grep -E '^kanedias-incus-profile-' && exit 1 || true
git log --oneline --decorate -5
```

Expected: the worktree is clean, no temporary live-test resources remain, and the design, plan, and implementation commits are present.
