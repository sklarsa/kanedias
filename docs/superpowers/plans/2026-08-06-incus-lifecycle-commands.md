# Incus Lifecycle Commands Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the image, sandbox, and workspace shell workflows with context-aware Cobra commands using only the Incus Go client.

**Architecture:** `internal/incusclient` is a thin adapter over the local Incus Unix-socket client. Focused `internal/image`, `internal/sandbox`, and `internal/workspace` packages own workflow sequencing, while `internal/network` and `internal/profiles` provide shared desired state. Cobra only loads config and delegates.

**Tech Stack:** Go 1.26.5, Cobra, github.com/lxc/incus/v7 v7.3.0, Incus shared API types, Go embed, existing TOML and profile templates.

## Global Constraints

- Production code must never invoke the `incus` executable; use the Incus v7 Go client exclusively.
- Connect destination operations with `incus.ConnectIncusUnixWithContext(ctx, "", nil)`.
- Use an explicit SimpleStreams source URL and image alias from `[base_image]`; do not load named Incus remotes.
- Every request and operation wait must use the Cobra command context.
- Cancellation must stop new work. Cleanup must use `context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)`.
- Ensure `image-build` before image launch; ensure the managed network and `sandbox` profile before sandbox/workspace launch.
- Keep the implementation direct and clean. Do not add a generic orchestration engine, retries, configurable timeout framework, remote selection, or speculative edge-case handling.
- `base_image.name`, `base_image.source`, and `base_image.image` are required for lifecycle commands.
- `workspace.pool` is optional; when empty, require exactly one connected storage pool.
- `workspace.volume` defaults to `kanedias-workspace-seed`.
- Sandbox volumes are named `kanedias-workspace-<name>`.
- Managed user is the constant `kanedias`; readiness and DNS timeouts are 60 seconds.
- Empty `authorized_hosts` is valid. Empty `workspace.repos` warns and succeeds after ensuring the seed volume.
- Preserve destructive repository refresh behavior.
- Embed only `internal/image/install.sh`; committed files under `assets/` remain disk inputs resolved beside the config file.
- Ignore `config.toml` and remove it from Git tracking without losing the user's local file.
- Delete migrated workflow scripts and replace their shell harnesses with Go tests where appropriate.
- Use managed worktrees for every writer. After the base lands, parallelize profiles and the Incus adapter; later parallelize image, sandbox, and workspace.
- Per user direction, run no task-level review agents. Run one independent final review after all implementation commits are merged and freshly verified.

---

## File Structure

### Shared configuration and client

- Modify `internal/config/config.go` and tests: lifecycle structs, defaults, config directory, validation.
- Create `internal/incusclient/client.go`: local connection, context-bound API calls and operation waits.
- Create `internal/incusclient/profile.go`: create-or-update profile from embedded YAML.
- Create `internal/incusclient/instance.go`: instance lifecycle, exec, upload, and publishing helpers.
- Create `internal/incusclient/storage.go`: pool resolution and custom-volume operations.
- Create tests beside each adapter file using an HTTP test server or narrow fake operation seams where practical.
- Modify `internal/network/network.go` and tests: use a narrow Go-client interface; remove command execution.
- Modify `internal/profiles/sandbox.yaml`, `profiles.go`, and tests: managed NIC, no inherited workspace disk, dynamic default CA path.

### Workflow packages

- Create `internal/image/image.go` and `image_test.go`; move `install.sh` to `internal/image/install.sh` and embed it.
- Create `internal/sandbox/sandbox.go`, `lock.go`, and tests.
- Create `internal/workspace/workspace.go`, `repositories.go`, and tests.

### Commands and cleanup

- Create `cmd/image.go`, `cmd/sandbox.go`, and `cmd/workspace.go`.
- Modify `cmd/root.go` and `cmd/root_test.go`.
- Remove migrated shell workflows and shell harnesses in their owning package tasks.
- Modify `.gitignore`; remove tracked `config.toml` while preserving the user's local ignored copy outside managed worktrees.
- Modify `go.mod` and `go.sum` for Incus v7.

---

### Task 1: Configuration Base and Local Config Tracking

**Execution:** One worker worktree. This commit must merge before parallel package work.

**Files:**
- Modify: `.gitignore`
- Remove from Git tracking: `config.toml`
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**

```go
type Config struct {
    Network   Network   `toml:"network"`
    BaseImage BaseImage `toml:"base_image"`
    Workspace Workspace `toml:"workspace"`
    Dir       string    `toml:"-"`
}

type BaseImage struct {
    Name            string   `toml:"name"`
    Source          string   `toml:"source"`
    Image           string   `toml:"image"`
    AuthorizedHosts []string `toml:"authorized_hosts"`
}

type Workspace struct {
    Pool   string   `toml:"pool"`
    Volume string   `toml:"volume"`
    Repos  []string `toml:"repos"`
}

const DefaultWorkspaceVolume = "kanedias-workspace-seed"

func Load(path string) (Config, error)
func (Config) ValidateLifecycle() error
func (Config) AssetPath(name string) string
```

- [ ] **Step 1: Add failing config tests**

Add tests for a full file, default workspace volume, absolute config directory, lifecycle required fields, empty authorized hosts, and empty repos. A representative case is:

```go
func TestLoadLifecycleConfig(t *testing.T) {
    path := writeConfig(t, `[network]
ipv4 = "10.76.111.1/24"
[base_image]
name = "sandbox"
source = "https://images.linuxcontainers.org"
image = "debian/13"
[workspace]
repos = []
`)
    cfg, err := Load(path)
    if err != nil { t.Fatal(err) }
    if cfg.Workspace.Volume != DefaultWorkspaceVolume { t.Fatalf("volume = %q", cfg.Workspace.Volume) }
    if got := cfg.AssetPath("tmux.conf"); got != filepath.Join(filepath.Dir(path), "assets", "tmux.conf") { t.Fatalf("asset path = %q", got) }
    if err := cfg.ValidateLifecycle(); err != nil { t.Fatal(err) }
}
```

Table-test each missing `base_image` field and require the exact field name in the error. Existing network validation tests must remain green.

- [ ] **Step 2: Verify the config tests fail**

```bash
go test ./internal/config
```

Expected: FAIL because the lifecycle fields and methods do not exist.

- [ ] **Step 3: Implement minimal config support**

Use `filepath.Abs(path)` to set `Config.Dir`, apply the volume default in `Load`, and keep existing network validation. `ValidateLifecycle` checks only the three required strings. `AssetPath(name)` returns `filepath.Join(Config.Dir, "assets", name)`.

- [ ] **Step 4: Ignore and untrack local config**

Set `.gitignore` to:

```gitignore
.pi-subagents
config.toml
```

Run `git rm --cached config.toml` in the worktree. Do not add a sample config in this iteration.

- [ ] **Step 5: Verify and commit**

```bash
gofmt -w internal/config/*.go
go test ./internal/config
go test ./...
git diff --check
git add .gitignore internal/config
git add -u config.toml
git commit -m "feat: configure Incus lifecycle workflows"
```

Expected: tests pass; the worktree has no `config.toml`; commit includes its tracked deletion.

---

### Task 2: Thin Incus Client and Network Migration

**Execution:** Run in parallel with Task 3 after Task 1 merges.

**Files:**
- Create: `internal/incusclient/client.go`
- Create: `internal/incusclient/profile.go`
- Create: `internal/incusclient/instance.go`
- Create: `internal/incusclient/storage.go`
- Create: focused `*_test.go` files
- Modify: `internal/network/network.go`
- Modify: `internal/network/network_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces produced by `incusclient.Client`:**

```go
type Client struct { /* private incus.InstanceServer */ }

type ExecRequest struct {
    Command     []string
    Environment map[string]string
    Cwd         string
    Stdin       io.Reader
}

func Connect(ctx context.Context) (*Client, error)
func (c *Client) Disconnect()
func (c *Client) ResolvePool(ctx context.Context, configured string) (string, error)
func (c *Client) EnsureProfile(ctx context.Context, name string, definition []byte) error
func (c *Client) GetNetwork(ctx context.Context, name string) (*api.Network, error)
func (c *Client) CreateNetwork(ctx context.Context, network api.NetworksPost) error
func (c *Client) GetStorageVolume(ctx context.Context, pool, name string) (*api.StorageVolume, error)
func (c *Client) CreateStorageVolume(ctx context.Context, pool, name string) error
func (c *Client) CopyStorageVolume(ctx context.Context, pool, source, target string) error
func (c *Client) DeleteStorageVolume(ctx context.Context, pool, name string) error
func (c *Client) GetInstance(ctx context.Context, name string) (*api.Instance, string, error)
func (c *Client) CreateInstance(ctx context.Context, request api.InstancesPost) error
func (c *Client) UpdateInstance(ctx context.Context, name string, request api.InstancePut, etag string) error
func (c *Client) StartInstance(ctx context.Context, name string) error
func (c *Client) StopInstance(ctx context.Context, name string, force bool) error
func (c *Client) DeleteInstance(ctx context.Context, name string) error
func (c *Client) Exec(ctx context.Context, name string, request ExecRequest) (stdout, stderr string, err error)
func (c *Client) PushFile(ctx context.Context, name, path string, content []byte, mode int) error
func (c *Client) PublishInstance(ctx context.Context, name, alias, description string) error
func IsNotFound(error) bool
```

`PublishInstance` may delete an existing alias before publishing with that alias. Do not build rollback machinery for alias replacement in this iteration.

- [ ] **Step 1: Add Incus v7 and failing adapter tests**

```bash
go get github.com/lxc/incus/v7@v7.3.0
```

Test the direct logic that does not require a daemon:

- configured pool returns unchanged;
- empty pool accepts exactly one returned name and rejects zero/multiple;
- profile YAML decodes into `api.ProfilePut` and create/update paths are selected;
- `Exec` captures stdout/stderr;
- operation wrappers pass the supplied context to `WaitContext`;
- remote volume wait cancels its target on context cancellation.

Use small private seams around `incus.Operation`, `incus.RemoteOperation`, and only the server calls each test needs. Do not implement a fake for the entire upstream `InstanceServer` interface.

- [ ] **Step 2: Verify red**

```bash
go test ./internal/incusclient
```

Expected: FAIL because the adapter does not exist.

- [ ] **Step 3: Implement the adapter directly**

Every method obtains a context-bound server with `c.server.WithContext(ctx)`. Local operations call `WaitContext(ctx)`. For `RemoteOperation`, wait in one buffered goroutine and call `CancelTarget` when `ctx.Done()` wins; return `ctx.Err()`.

Use:

```go
func Connect(ctx context.Context) (*Client, error) {
    server, err := incus.ConnectIncusUnixWithContext(ctx, "", nil)
    if err != nil { return nil, fmt.Errorf("connect to Incus: %w", err) }
    return &Client{server: server}, nil
}
```

`CreateInstance` accepts remote images through `api.InstanceSource{Type:"image", Server:cfg.BaseImage.Source, Protocol:"simplestreams", Alias:cfg.BaseImage.Image, Mode:"pull"}` supplied by the image workflow; the adapter does not parse remote names.

- [ ] **Step 4: Replace network command execution**

Change network reconciliation to use a narrow interface matching `GetNetwork` and `CreateNetwork`. Keep the public convenience function opening a client and add a shared-client function:

```go
func Ensure(ctx context.Context, cfg config.Config) error
func EnsureWithClient(ctx context.Context, client Client, cfg config.Config) error
```

Preserve existing create/validate behavior and tests, replacing fake command output with fake API objects. Delete all `os/exec` use from `internal/network`.

- [ ] **Step 5: Verify and commit**

```bash
gofmt -w internal/incusclient internal/network
go test ./internal/incusclient ./internal/network
go test ./...
go vet ./...
git diff --check
git commit -am "feat: use Incus Go client"
git add internal/incusclient go.mod go.sum
git commit --amend --no-edit
```

Expected: all tests pass and `rg 'exec.Command.*incus' internal cmd` returns no production match.

---

### Task 3: Lifecycle-Ready Embedded Profiles

**Execution:** Run in parallel with Task 2 after Task 1 merges.

**Files:**
- Modify: `internal/profiles/sandbox.yaml`
- Modify: `internal/profiles/profiles.go`
- Modify: `internal/profiles/profiles_test.go`

**Interfaces:** Existing `Render(io.Writer, string, config.Config) error` remains stable.

- [ ] **Step 1: Add failing sandbox profile assertions**

Require rendered sandbox YAML to contain:

```yaml
  eth0:
    name: eth0
    network: kanedias
    type: nic
```

Require it not to contain a `workspace:` device. Set `XDG_CONFIG_HOME` in the test and require the proxy CA source to be `<XDG_CONFIG_HOME>/kanedias-proxy/ca.crt`.

- [ ] **Step 2: Verify red**

```bash
go test ./internal/profiles
```

Expected: FAIL on NIC, workspace, and CA-source assertions.

- [ ] **Step 3: Update the template and render data**

Add `ProxyCACertPath` to private template data, derive it from `os.UserConfigDir`, and template the proxy CA source. Remove the workspace device. Add the `eth0` device above.

- [ ] **Step 4: Verify and commit**

```bash
gofmt -w internal/profiles/*.go
go test ./internal/profiles
go test ./...
git diff --check
git add internal/profiles
git commit -m "feat: prepare lifecycle profiles"
```

---

### Task 4: Image Creation Workflow

**Execution:** Run in parallel with Tasks 5 and 6 after Tasks 2 and 3 merge.

**Files:**
- Create: `internal/image/image.go`
- Create: `internal/image/image_test.go`
- Move: `install.sh` → `internal/image/install.sh`
- Delete: `build-image.sh`
- Delete: `test-install.sh`

**Public interface:**

```go
func Create(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) error
```

**Private test seam:** a narrow client interface containing only image workflow calls from Task 2.

- [ ] **Step 1: Move and embed the installer**

```bash
mkdir -p internal/image
git mv install.sh internal/image/install.sh
```

In `image.go`:

```go
//go:embed install.sh
var installer []byte
```

- [ ] **Step 2: Add failing happy-path and cleanup tests**

Test this order with a recording fake:

```text
ensure-profile image-build
create-instance
push /root/install.sh
push /root/assets/authorized_hosts
push /root/assets/pi-settings.json
push /root/assets/cobalt-ember.json
push /root/assets/tmux.conf
exec bash /root/install.sh
stop
publish
cleanup-delete-instance
```

Assert the instance source uses configured URL, `simplestreams`, and configured image alias. Assert authorized hosts are newline-joined and empty input produces an empty file. Add one cancellation/failure test proving cleanup receives a context not already canceled.

- [ ] **Step 3: Verify red**

```bash
go test ./internal/image
```

Expected: FAIL because `Create` is absent.

- [ ] **Step 4: Implement the direct workflow**

Read the three disk assets before connecting. Validate lifecycle config before side effects. Use a unique `image-build-<timestamp>-<pid>` name, `default` plus `image-build` profiles, and remote image source fields. Defer cleanup immediately after successful creation. Upload files with `PushFile`, execute `bash /root/install.sh`, stop, publish with description `kanedias sandbox from <source>/<image>`, then delete the temporary instance using the bounded cleanup context.

- [ ] **Step 5: Remove shell workflow and verify**

Delete `build-image.sh` and `test-install.sh`. Add an optional `//go:build incus` smoke test that calls `Create` only when `KANEDIAS_LIVE_IMAGE_CREATE=1`; otherwise skip.

```bash
gofmt -w internal/image/*.go
go test ./internal/image
go test ./...
git diff --check
git add -A -- internal/image install.sh build-image.sh test-install.sh
git commit -m "feat: create images through Incus client"
```

---

### Task 5: Sandbox Lifecycle Workflow

**Execution:** Run in parallel with Tasks 4 and 6 after shared packages merge.

**Files:**
- Create: `internal/sandbox/sandbox.go`
- Create: `internal/sandbox/lock.go`
- Create: `internal/sandbox/sandbox_test.go`
- Delete: `launch-sandbox.sh`
- Delete: `remove_sandbox.sh`
- Delete: `test-launch-sandbox.sh`
- Delete: `test-remove-sandbox.sh`

**Public interfaces:**

```go
func Create(ctx context.Context, cfg config.Config, name string, stdout, stderr io.Writer) error
func Destroy(ctx context.Context, cfg config.Config, name string, stdout, stderr io.Writer) error
```

- [ ] **Step 1: Add failing create tests**

Use a recording fake and require:

```text
resolve-pool
lock
init-ca
ensure-network
ensure-profile sandbox
get-seed
copy seed -> kanedias-workspace-<name>
create-instance with local workspace device
start
exec systemctl is-system-running --wait
exec update-ca-certificates
```

Assert a failure after volume creation cleans only the owned instance/volume with a non-cancelled bounded context.

- [ ] **Step 2: Add failing destroy tests**

Cover the useful paths only:

- existing instance with expected local workspace source deletes instance before volume;
- mismatched/missing local workspace device fails without deletion;
- both absent succeeds;
- seed volume can never be selected for deletion.

- [ ] **Step 3: Verify red**

```bash
go test ./internal/sandbox
```

Expected: FAIL because package implementation is absent.

- [ ] **Step 4: Implement lifecycle and lock**

Use a non-blocking Unix flock at `filepath.Join(os.TempDir(), fmt.Sprintf("kanedias-sandbox-locks-%d", os.Getuid()), name+".lock")`; create the directory mode `0700`. Validate name only for empty, `/`, `.`, and `..`, matching the old workflow without adding a new validation framework.

Create calls `proxy.DefaultOptions`/`InitCA`, `network.EnsureWithClient`, renders and ensures the sandbox profile, copies the seed, creates the instance from the local alias `base_image.name`, and waits up to 60 seconds for systemd. Accept stdout `running` or `degraded`.

Destroy gets local devices from `api.Instance.Devices`, verifies `workspace["source"]`, deletes the instance operation first, then the custom volume. Use `api.StatusErrorCheck(err, http.StatusNotFound)` through `incusclient.IsNotFound`.

- [ ] **Step 5: Remove shell workflows and verify**

```bash
rm launch-sandbox.sh remove_sandbox.sh test-launch-sandbox.sh test-remove-sandbox.sh
gofmt -w internal/sandbox/*.go
go test ./internal/sandbox
go test ./...
git diff --check
git add -A -- internal/sandbox launch-sandbox.sh remove_sandbox.sh test-launch-sandbox.sh test-remove-sandbox.sh
git commit -m "feat: manage sandbox lifecycle"
```

---

### Task 6: Workspace Synchronization Workflow

**Execution:** Run in parallel with Tasks 4 and 5 after shared packages merge.

**Files:**
- Create: `internal/workspace/workspace.go`
- Create: `internal/workspace/repositories.go`
- Create: `internal/workspace/workspace_test.go`
- Delete: `sync-workspace.sh`
- Delete: `test-sync-workspace.sh`

**Public interface:**

```go
func Sync(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) error
```

- [ ] **Step 1: Add failing validation and empty-list tests**

Validate `owner/repository` shape and duplicate basename before client connection. For an empty list, require pool resolution and seed-volume creation/reuse, a warning on stderr, and no instance creation.

- [ ] **Step 2: Add failing happy-path test**

Require this broad order:

```text
resolve-pool
get/create seed
init-ca
ensure-network
ensure-profile sandbox
create temporary instance with seed local device
start
exec DNS check
exec update-ca-certificates
prepare /workspace/repos
configure gh/git
clone or refresh each repository
stop
remove local workspace device
update instance
delete instance
```

For an existing repository, assert commands include fetch with prune/tags, remote-head resolution, forced branch/reset/clean, submodule sync/update, and recursive reset/clean. Add one cancellation test proving bounded cleanup still runs.

- [ ] **Step 3: Verify red**

```bash
go test ./internal/workspace
```

Expected: FAIL because `Sync` is absent.

- [ ] **Step 4: Implement repository synchronization**

Keep commands explicit. Use `runuser -u kanedias -- env HOME=/home/kanedias USER=kanedias LOGNAME=kanedias` as the command prefix. Use separate `Exec` calls rather than copying a script. Query path state with `test`, obtain the default remote ref from `git symbolic-ref`, and then issue the existing destructive Git commands. Do not add retry/backoff or concurrency.

DNS readiness loops until the 60-second deadline, running `getent ahosts github.com`; exit immediately on context cancellation. Cleanup stops the instance, removes its local workspace device via `GetInstance`/`UpdateInstance`, and deletes it under the bounded cleanup context.

- [ ] **Step 5: Remove shell workflow and verify**

```bash
rm sync-workspace.sh test-sync-workspace.sh
gofmt -w internal/workspace/*.go
go test ./internal/workspace
go test ./...
git diff --check
git add -A -- internal/workspace sync-workspace.sh test-sync-workspace.sh
git commit -m "feat: synchronize workspace through Incus client"
```

---

### Task 7: Merge Parallel Workflow Lanes

**Execution:** Parent orchestrator on `main`; no review yet.

- [ ] **Step 1: Merge image, sandbox, and workspace commits**

Cherry-pick the exact handoff commits one at a time. The lanes own disjoint workflow directories and shell paths.

- [ ] **Step 2: Verify package integration**

```bash
go test ./...
go vet ./...
git diff --check
rg -n 'exec\.Command.*incus|CommandContext\([^\n]*"incus"' --glob '*.go' . && exit 1 || true
```

Expected: all Go checks pass and no production Incus CLI invocation exists.

---

### Task 8: Cobra Command Wiring

**Execution:** One worker worktree after all domain workflows merge.

**Files:**
- Create: `cmd/image.go`
- Create: `cmd/sandbox.go`
- Create: `cmd/workspace.go`
- Modify: `cmd/root.go`
- Modify: `cmd/root_test.go`

**Interfaces:** Add service functions matching the four workflow public functions. Existing proxy/profile services stay unchanged.

- [ ] **Step 1: Add failing hierarchy and delegation tests**

Require exact resolution and default arguments:

```text
image create
sandbox create
sandbox create personal
sandbox destroy
sandbox destroy personal
workspace sync
```

Assert no lifecycle flags. Use a canceled command context and require the exact context reaches each fake service. Assert config load occurs once before delegation and errors propagate without extra calls.

- [ ] **Step 2: Verify red**

```bash
go test ./cmd
```

Expected: FAIL because constructors/services are absent.

- [ ] **Step 3: Implement small Cobra handlers**

Each parent command is a group. Each leaf uses `cobra.MaximumNArgs(1)` for sandbox or `cobra.NoArgs` otherwise. Sandbox defaults the name to `sandbox`. Handlers load config and call the corresponding service with `cmd.Context()`, `cmd.OutOrStdout()`, and `cmd.ErrOrStderr()`.

Update root help to mention Incus lifecycle management. Leave completion enabled.

- [ ] **Step 4: Verify and commit**

```bash
gofmt -w cmd/*.go
go test ./cmd
go test ./...
go vet ./...
git diff --check
go run . image --help
go run . sandbox --help
go run . workspace --help
git add cmd
git commit -m "feat: add Incus lifecycle commands"
```

---

### Task 9: Restore Local Config and Full Verification

**Execution:** Parent orchestrator after Task 8 merges.

- [ ] **Step 1: Restore the user's ignored config with explicit source fields**

Restore the pre-execution backup, preserving their network, authorized-host, and repository values. Ensure it contains:

```toml
[base_image]
name = "sandbox"
source = "https://images.linuxcontainers.org"
image = "debian/13"
```

Keep `config.toml` ignored and untracked. Do not print authorized-host contents in logs or final output.

- [ ] **Step 2: Run fresh non-live verification**

```bash
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
git diff --check
```

Smoke command help and config parsing without executing live lifecycle mutations:

```bash
go run . image create --help
go run . sandbox create --help
go run . sandbox destroy --help
go run . workspace sync --help
go run . --config ./config.toml profile sandbox >/tmp/kanedias-profile.yaml
```

Confirm no legacy workflow scripts remain, `internal/image/install.sh` is embedded, config is ignored, assets remain tracked, and no production Go source invokes the Incus CLI.

---

### Task 10: One Final Independent Review

**Execution:** Only after all commits are merged and Task 9 is green. One fresh read-only reviewer in a managed worktree; no earlier review agents.

- [ ] **Step 1: Generate the complete review package**

Use the implementation base and current head. Give the reviewer the approved spec, this plan, complete diff, and fresh verification output.

Require explicit checks for:

- Go-client-only Incus production paths;
- command context reaching requests, operation waits, exec, and DNS/readiness loops;
- non-cancelled 30-second cleanup contexts;
- profile ensure before every launch;
- network and NIC setup before sandbox/workspace launch;
- correct local workspace device and volume naming;
- image installer embedding and full publish workflow;
- empty repo warning behavior and destructive refresh sequence;
- removed shell workflows/tests and retained committed assets;
- ignored/untracked config preservation.

- [ ] **Step 2: Apply findings once if needed**

If the reviewer reports Critical or Important findings, dispatch one fix worker in a managed worktree with the complete finding list. Require reproducing tests, focused fixes, full verification, and one commit. Merge it and run one scoped final re-review. Do not launch per-finding workers or multiple review rounds.

- [ ] **Step 3: Final merged-main verification**

```bash
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
git diff --check
git status --short --untracked-files=all
git log --oneline --decorate -12
```

Expected: all checks pass; `config.toml` does not appear because it is ignored; all implementation commits are on `main`; no managed worktree remains unmerged.
