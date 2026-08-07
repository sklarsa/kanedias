# Native Nested Incus Workspace PoC Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build and maintain a cold native-Btrfs nested-Incus seed, clone it into manually managed sandboxes at `/var/lib/incus`, and prove that concurrent sandbox clones run isolated nested daemons.

**Architecture:** A new `internal/workspace/incus` package owns seed locking, seed synchronization, native-Btrfs validation, clone naming, and clone/device helpers. The CLI exposes separate `workspace repos sync` and `workspace incus sync` commands. Manual sandbox lifecycle consumes the helper package, while all session and session-supervisor files remain untouched.

**Tech Stack:** Go 1.24, Cobra, Incus v7 Go client/API, Linux `flock`, systemd, Btrfs, table-driven Go tests, opt-in live Incus tests.

## Global Constraints

- Do not modify `internal/session/**`, `cmd/session.go`, or `docs/architecture/session-supervisor.md`.
- Keep the repository custom volume mounted at `/workspace`; repositories remain under `/workspace/repos`.
- Mount the nested-Incus custom volume directly at `/var/lib/incus`; do not use a symlink, bind mount, or alternate `INCUS_DIR`.
- Require the resolved outer storage pool driver to be exactly `btrfs`.
- Require the inner default storage pool driver to be exactly `btrfs` with a native source under `/var/lib/incus/storage-pools`; reject loop image sources under `/var/lib/incus/disks`.
- Keep outer sandboxes unprivileged and retain `security.nesting=true`.
- Do not retain `kanedias workspace sync`; the only workspace sync commands are `workspace repos sync` and `workspace incus sync`.
- Defer cron scheduling, immutable generations, generation retention, disk-pressure protection, and strict quotas.
- One nested daemon exclusively owns one cloned Incus-state volume; never attach the seed or a clone to multiple running maintenance/sandbox instances.

---

## File Structure

### New files

- `internal/workspace/incus/lock.go` — shared/exclusive host file lock for cold seed copies versus seed mutation.
- `internal/workspace/incus/lock_test.go` — lock compatibility and permissions tests.
- `internal/workspace/incus/state.go` — seed/clone naming, disk-device construction, clone result tracking, and clone/delete operations.
- `internal/workspace/incus/state_test.go` — naming, device, missing-seed, submitted-operation, and clone tests.
- `internal/workspace/incus/inner.go` — commands and parsers for initializing, validating, populating, and quiescing the nested daemon.
- `internal/workspace/incus/inner_test.go` — exact command and storage-pool validation tests.
- `internal/workspace/incus/sync.go` — maintenance-container lifecycle for `workspace incus sync`.
- `internal/workspace/incus/sync_test.go` — orchestration, cancellation, rollback, and cold-seed tests.
- `internal/workspace/incus/live_incus_test.go` — opt-in two-sandbox native-Btrfs isolation test.

### Modified files

- `config.toml` — configure the Incus-state seed and curated inner image.
- `internal/config/config.go` — nested workspace Incus configuration and default volume.
- `internal/config/config_test.go` — decoding and default tests.
- `internal/incusclient/storage.go` — context-aware outer storage-pool lookup.
- `internal/incusclient/storage_test.go` — storage-pool adapter tests.
- `internal/profiles/sandbox.yaml` — nested `mknod` and `setxattr` interception.
- `internal/profiles/profiles_test.go` — nesting policy assertions.
- `cmd/root.go` — separate repository and Incus synchronization services.
- `cmd/workspace.go` — nested `repos sync` and `incus sync` command hierarchy.
- `cmd/root_test.go` — hierarchy, delegation, argument, and config failure tests.
- `internal/sandbox/sandbox.go` — clone, attach, verify, roll back, and delete private Incus state.
- `internal/sandbox/sandbox_test.go` — dual-volume lifecycle and ownership tests.

---

### Task 1: Add Nested-Incus Workspace Configuration

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `config.toml`

**Interfaces:**
- Produces: `config.IncusWorkspace` with fields `Volume string` and `Images []string`.
- Produces: `config.DefaultIncusWorkspaceVolume == "kanedias-incus-seed"`.
- Produces: `config.Workspace.Incus config.IncusWorkspace`.
- Preserves: `config.Workspace.Pool`, `Volume`, and `Repos` so existing session code compiles unchanged.

- [ ] **Step 1: Extend lifecycle config decoding tests**

Add an Incus subsection to `TestLoadLifecycleConfig` and assert the decoded value:

```go
[workspace]
pool = "default"
volume = "workspace"
repos = ["owner/repo", "other/project"]
[workspace.incus]
volume = "nested-state"
images = ["images:debian/13", "images:ubuntu/24.04"]
```

Update the existing full `Workspace` expectation so it includes:

```go
Incus: IncusWorkspace{
    Volume: "nested-state",
    Images: []string{"images:debian/13", "images:ubuntu/24.04"},
},
```

Keep a focused assertion as well:

```go
if got := cfg.Workspace.Incus.Volume; got != "nested-state" {
    t.Errorf("Workspace.Incus.Volume = %q, want nested-state", got)
}
```

Extend `TestLoadLifecycleDefaultsAndPaths`:

```go
if got := cfg.Workspace.Incus.Volume; got != DefaultIncusWorkspaceVolume {
    t.Fatalf("Workspace.Incus.Volume = %q, want %q", got, DefaultIncusWorkspaceVolume)
}
if cfg.Workspace.Incus.Images != nil {
    t.Fatalf("Workspace.Incus.Images = %#v, want nil", cfg.Workspace.Incus.Images)
}
```

- [ ] **Step 2: Run the focused config tests and verify failure**

Run:

```bash
go test ./internal/config -run 'TestLoadLifecycle(Config|DefaultsAndPaths)$' -count=1
```

Expected: compilation fails because `IncusWorkspace`, `Workspace.Incus`, and `DefaultIncusWorkspaceVolume` do not exist.

- [ ] **Step 3: Add the nested configuration types and default**

Update `internal/config/config.go`:

```go
type Workspace struct {
    Pool   string         `toml:"pool"`
    Volume string         `toml:"volume"`
    Repos  []string       `toml:"repos"`
    Incus  IncusWorkspace `toml:"incus"`
}

type IncusWorkspace struct {
    Volume string   `toml:"volume"`
    Images []string `toml:"images"`
}

const (
    DefaultWorkspaceVolume      = "kanedias-workspace-seed"
    DefaultIncusWorkspaceVolume = "kanedias-incus-seed"
)
```

After the existing repository-volume default in `Load`, add:

```go
if cfg.Workspace.Incus.Volume == "" {
    cfg.Workspace.Incus.Volume = DefaultIncusWorkspaceVolume
}
```

- [ ] **Step 4: Configure the checked-in PoC seed**

Fix the missing comma between the final repository entries in `config.toml`, then add:

```toml
[workspace.incus]
volume = "kanedias-incus-seed"
images = [
    "images:debian/13",
]
```

Verify the checked-in config parses:

```bash
go test ./internal/config -count=1
go run . --config ./config.toml profile sandbox >/dev/null
```

Expected: both commands exit successfully.

- [ ] **Step 5: Commit the configuration change**

```bash
git add config.toml internal/config/config.go internal/config/config_test.go
git commit -m "feat: configure nested Incus workspace state"
```

---

### Task 2: Add Outer Storage Introspection and Nested Profile Permissions

**Files:**
- Modify: `internal/incusclient/storage.go`
- Modify: `internal/incusclient/storage_test.go`
- Modify: `internal/profiles/sandbox.yaml`
- Modify: `internal/profiles/profiles_test.go`

**Interfaces:**
- Produces: `(*incusclient.Client).GetStoragePool(context.Context, string) (*api.StoragePool, error)`.
- Produces: an outer sandbox profile containing `security.syscalls.intercept.mknod=true` and `security.syscalls.intercept.setxattr=true`.
- Preserves: `security.privileged=false` and `security.nesting=true`.

- [ ] **Step 1: Write failing storage-pool adapter tests**

Add a narrow fake and test to `internal/incusclient/storage_test.go`:

```go
type fakeStoragePoolGetter struct {
    name string
    pool *api.StoragePool
    err  error
}

func (f *fakeStoragePoolGetter) GetStoragePool(name string) (*api.StoragePool, string, error) {
    f.name = name
    return f.pool, "etag", f.err
}

func TestGetStoragePool(t *testing.T) {
    fake := &fakeStoragePoolGetter{pool: &api.StoragePool{Name: "pool1", Driver: "btrfs"}}
    pool, err := getStoragePool(fake, "pool1")
    if err != nil {
        t.Fatal(err)
    }
    if fake.name != "pool1" || pool.Driver != "btrfs" {
        t.Fatalf("GetStoragePool() = %#v after name %q", pool, fake.name)
    }
}

func TestGetStoragePoolWrapsError(t *testing.T) {
    sentinel := errors.New("storage unavailable")
    _, err := getStoragePool(&fakeStoragePoolGetter{err: sentinel}, "pool1")
    if !errors.Is(err, sentinel) || !strings.Contains(err.Error(), `storage pool "pool1"`) {
        t.Fatalf("getStoragePool() error = %v", err)
    }
}
```

Add imports for `strings` and `github.com/lxc/incus/v7/shared/api`.

- [ ] **Step 2: Run the storage tests and verify failure**

Run:

```bash
go test ./internal/incusclient -run 'TestGetStoragePool' -count=1
```

Expected: compilation fails because `getStoragePool` does not exist.

- [ ] **Step 3: Implement context-aware pool lookup**

Add to `internal/incusclient/storage.go`:

```go
type storagePoolGetter interface {
    GetStoragePool(string) (*api.StoragePool, string, error)
}

func getStoragePool(server storagePoolGetter, name string) (*api.StoragePool, error) {
    pool, _, err := server.GetStoragePool(name)
    if err != nil {
        return nil, fmt.Errorf("get Incus storage pool %q: %w", name, err)
    }
    return pool, nil
}

func (c *Client) GetStoragePool(ctx context.Context, name string) (*api.StoragePool, error) {
    return getStoragePool(c.server.WithContext(ctx), name)
}
```

Run:

```bash
go test ./internal/incusclient -run 'TestGetStoragePool' -count=1
```

Expected: PASS.

- [ ] **Step 4: Write failing profile policy assertions**

In `TestRenderSandboxUsesLifecycleDevicesAndDefaultProxyCA`, add:

```go
for _, want := range []string{
    `  security.nesting: "true"`,
    `  security.privileged: "false"`,
    `  security.syscalls.intercept.mknod: "true"`,
    `  security.syscalls.intercept.setxattr: "true"`,
} {
    if !strings.Contains(rendered, want) {
        t.Errorf("rendered sandbox missing %q", want)
    }
}
```

Run:

```bash
go test ./internal/profiles -run TestRenderSandboxUsesLifecycleDevicesAndDefaultProxyCA -count=1
```

Expected: FAIL because the two syscall interception keys are absent.

- [ ] **Step 5: Add the required unprivileged nesting keys**

Update `internal/profiles/sandbox.yaml`:

```yaml
  security.nesting: "true"
  security.privileged: "false"
  security.syscalls.intercept.mknod: "true"
  security.syscalls.intercept.setxattr: "true"
```

Run:

```bash
go test ./internal/incusclient ./internal/profiles -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the platform prerequisites**

```bash
git add internal/incusclient/storage.go internal/incusclient/storage_test.go internal/profiles/sandbox.yaml internal/profiles/profiles_test.go
git commit -m "feat: prepare sandboxes for native nested Incus"
```

---

### Task 3: Implement Seed Locking and Clone Primitives

**Files:**
- Create: `internal/workspace/incus/lock.go`
- Create: `internal/workspace/incus/lock_test.go`
- Create: `internal/workspace/incus/state.go`
- Create: `internal/workspace/incus/state_test.go`

**Interfaces:**
- Produces: `incusworkspace.DeviceName == "incus-state"` and `incusworkspace.MountPath == "/var/lib/incus"`.
- Produces: `SeedVolume(config.Config) string` and `SandboxVolume(string) string`.
- Produces: `Device(pool, volume string) map[string]string`.
- Produces: `CloneResult{Name string, Created bool}`.
- Produces: `Clone(context.Context, VolumeClient, pool, seed, sandbox string) (CloneResult, error)`.
- Produces: `Delete(context.Context, VolumeClient, pool, seed, volume string) error`.
- Consumes: `config.DefaultIncusWorkspaceVolume` and `incusclient.OperationWasSubmitted`.

- [ ] **Step 1: Write lock compatibility tests**

Create `internal/workspace/incus/lock_test.go` with tests that acquire two shared locks, reject an exclusive lock while either shared lock is held, then acquire an exclusive lock after both close:

```go
func TestSeedLockAllowsReadersAndExcludesWriter(t *testing.T) {
    pool := "pool-" + strings.ReplaceAll(t.Name(), "/", "-")
    first, err := acquireSeedLock(pool, "seed", false)
    if err != nil {
        t.Fatal(err)
    }

    second, err := acquireSeedLock(pool, "seed", false)
    if err != nil {
        t.Fatal(err)
    }

    if writer, err := acquireSeedLock(pool, "seed", true); err == nil {
        writer.Close()
        t.Fatal("exclusive seed lock succeeded while shared locks were held")
    }

    if err := second.Close(); err != nil {
        t.Fatal(err)
    }
    if err := first.Close(); err != nil {
        t.Fatal(err)
    }

    writer, err := acquireSeedLock(pool, "seed", true)
    if err != nil {
        t.Fatal(err)
    }
    defer writer.Close()
}
```

Add a second test that inspects the lock directory and requires mode `0700` and lock file mode `0600`.

- [ ] **Step 2: Run the lock tests and verify failure**

Run:

```bash
go test ./internal/workspace/incus -run TestSeedLock -count=1
```

Expected: compilation fails because `acquireSeedLock` does not exist.

- [ ] **Step 3: Implement shared/exclusive non-blocking locks**

Create `internal/workspace/incus/lock.go`. Use a SHA-256 digest of `pool + "\x00" + seed` as the lock filename so configured names cannot escape the private directory:

```go
func acquireSeedLock(pool, seed string, exclusive bool) (io.Closer, error) {
    dir := filepath.Join(os.TempDir(), fmt.Sprintf("kanedias-incus-seed-locks-%d", os.Getuid()))
    if err := os.MkdirAll(dir, 0o700); err != nil {
        return nil, fmt.Errorf("create nested Incus seed lock directory: %w", err)
    }
    if err := os.Chmod(dir, 0o700); err != nil {
        return nil, fmt.Errorf("set nested Incus seed lock directory permissions: %w", err)
    }

    digest := sha256.Sum256([]byte(pool + "\x00" + seed))
    path := filepath.Join(dir, hex.EncodeToString(digest[:]) + ".lock")
    file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
    if err != nil {
        return nil, fmt.Errorf("open nested Incus seed lock: %w", err)
    }

    operation := unix.LOCK_SH | unix.LOCK_NB
    if exclusive {
        operation = unix.LOCK_EX | unix.LOCK_NB
    }
    if err := unix.Flock(int(file.Fd()), operation); err != nil {
        file.Close()
        if errors.Is(err, unix.EWOULDBLOCK) {
            return nil, fmt.Errorf("another operation is active for nested Incus seed %q", seed)
        }
        return nil, fmt.Errorf("lock nested Incus seed %q: %w", seed, err)
    }
    return &seedLock{file: file}, nil
}
```

Implement `seedLock.Close` by joining unlock and close errors, matching `internal/sandbox/lock.go`.

Run:

```bash
go test ./internal/workspace/incus -run TestSeedLock -count=1
```

Expected: PASS.

- [ ] **Step 4: Write clone and device tests**

Create `internal/workspace/incus/state_test.go` with a recording client and these assertions:

```go
func TestStateNamesAndDevice(t *testing.T) {
    cfg := config.Config{Workspace: config.Workspace{Incus: config.IncusWorkspace{Volume: "seed"}}}
    if got := SeedVolume(cfg); got != "seed" {
        t.Fatalf("SeedVolume() = %q", got)
    }
    if got := SandboxVolume("demo"); got != "kanedias-incus-demo" {
        t.Fatalf("SandboxVolume() = %q", got)
    }
    want := map[string]string{
        "type": "disk", "pool": "pool1", "source": "kanedias-incus-demo", "path": "/var/lib/incus",
    }
    if got := Device("pool1", "kanedias-incus-demo"); !reflect.DeepEqual(got, want) {
        t.Fatalf("Device() = %#v, want %#v", got, want)
    }
}
```

Add tests for:

- a missing seed returns the wrapped `GetStorageVolume` error and never calls copy;
- a seed whose `api.StorageVolume.UsedBy` is non-empty returns `nested Incus seed %q is attached and cannot be cloned` and never calls copy;
- successful copy returns `CloneResult{Name: "kanedias-incus-demo", Created: true}`;
- a pre-submission copy error returns `Created: false`;
- an `incusclient.OperationWasSubmitted` copy error returns `Created: true` so callers clean up an ambiguous target;
- the call sequence is `get seed`, then `copy seed target` while the shared lock is held.

- [ ] **Step 5: Run state tests and verify failure**

Run:

```bash
go test ./internal/workspace/incus -run 'Test(State|Clone)' -count=1
```

Expected: compilation fails because the state interfaces do not exist.

- [ ] **Step 6: Implement clone state helpers**

Create `internal/workspace/incus/state.go`:

```go
package incusworkspace

const (
    DeviceName = "incus-state"
    MountPath  = "/var/lib/incus"
    volumePrefix = "kanedias-incus-"
)

type VolumeClient interface {
    GetStorageVolume(context.Context, string, string) (*api.StorageVolume, error)
    CopyStorageVolume(context.Context, string, string, string) error
    DeleteStorageVolume(context.Context, string, string) error
}

type CloneResult struct {
    Name    string
    Created bool
}

func SeedVolume(cfg config.Config) string {
    if cfg.Workspace.Incus.Volume == "" {
        return config.DefaultIncusWorkspaceVolume
    }
    return cfg.Workspace.Incus.Volume
}

func SandboxVolume(name string) string { return volumePrefix + name }

func Device(pool, volume string) map[string]string {
    return map[string]string{
        "type": "disk", "pool": pool, "source": volume, "path": MountPath,
    }
}
```

Implement `Clone` in this exact order: acquire a shared seed lock, fetch the seed, require `len(seedVolume.UsedBy) == 0`, copy to `SandboxVolume(sandbox)`, set `Created=true` on success or when `incusclient.OperationWasSubmitted(err)` is true, and return the copy error unchanged. Add a thin `Delete` wrapper that refuses `volume == seed` before calling `DeleteStorageVolume`.

Run:

```bash
go test ./internal/workspace/incus -run 'Test(SeedLock|State|Clone)' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit the clone primitives**

```bash
git add internal/workspace/incus/lock.go internal/workspace/incus/lock_test.go internal/workspace/incus/state.go internal/workspace/incus/state_test.go
git commit -m "feat: add nested Incus state clone primitives"
```

---

### Task 4: Implement Inner Daemon Operations and Native-Btrfs Validation

**Files:**
- Create: `internal/workspace/incus/inner.go`
- Create: `internal/workspace/incus/inner_test.go`

**Interfaces:**
- Produces: `Executor` with `Exec(context.Context, string, incusclient.ExecRequest) (string, string, error)`.
- Produces: `WaitReady(context.Context, Executor, instance string, timeout time.Duration) error`.
- Produces: `VerifyNativeBtrfs(context.Context, Executor, instance string) error`.
- Produces: `initialize(context.Context, Executor, instance string, newSeed bool, timeout time.Duration) error`.
- Produces: `syncImages(context.Context, Executor, instance string, images []string) error`.
- Produces: `quiesce(context.Context, Executor, instance string) error`.

- [ ] **Step 1: Write native storage-pool validation tests**

Create `internal/workspace/incus/inner_test.go` with a recording executor. Cover these exact query payloads:

```go
{"name":"default","driver":"btrfs","config":{"source":"/var/lib/incus/storage-pools/default"}}
{"name":"default","driver":"dir","config":{"source":"/var/lib/incus/storage-pools/default"}}
{"name":"default","driver":"btrfs","config":{"source":"/var/lib/incus/disks/default.img"}}
{"name":"default","driver":"btrfs","config":{"source":"/other/default"}}
```

Require only the first payload to pass. Also require malformed JSON to return an error containing `decode nested Incus storage pool`.

- [ ] **Step 2: Write exact command tests**

Using the recording executor, add tests requiring:

```text
incus admin waitready --timeout 60
incus admin init --minimal
incus query /1.0/storage-pools/default
incus image copy images:debian/13 local: --copy-aliases --auto-update --reuse
systemctl stop incus.socket
systemctl stop incus.service
systemctl show --property=ActiveState --value incus.socket
systemctl show --property=ActiveState --value incus.service
systemctl show --property=MainPID --value incus.service
```

For `quiesce`, return `inactive`, `inactive`, and `0` from the final three commands and assert success. Add failure cases for an active socket, active service, and nonzero main PID.

- [ ] **Step 3: Run inner operation tests and verify failure**

Run:

```bash
go test ./internal/workspace/incus -run 'Test(VerifyNativeBtrfs|WaitReady|Initialize|SyncImages|Quiesce)' -count=1
```

Expected: compilation fails because the inner operation functions do not exist.

- [ ] **Step 4: Implement command execution and validation**

Create `internal/workspace/incus/inner.go` with:

```go
type Executor interface {
    Exec(context.Context, string, incusclient.ExecRequest) (string, string, error)
}

type innerStoragePool struct {
    Driver string            `json:"driver"`
    Config map[string]string `json:"config"`
}
```

`WaitReady` must derive a bounded context and execute:

```go
[]string{"incus", "admin", "waitready", "--timeout", strconv.Itoa(int(timeout.Seconds()))}
```

`VerifyNativeBtrfs` must execute `incus query /1.0/storage-pools/default`, JSON-decode stdout, require `Driver == "btrfs"`, clean the source path, require it to be strictly below `/var/lib/incus/storage-pools`, and reject any source below `/var/lib/incus/disks` or ending in `.img`.

`initialize` must call `WaitReady`; when `newSeed` is true, run `incus admin init --minimal` and call `WaitReady` again; it must then call `VerifyNativeBtrfs`.

`syncImages` must execute one image-copy command per configured reference in input order:

```go
[]string{"incus", "image", "copy", image, "local:", "--copy-aliases", "--auto-update", "--reuse"}
```

`quiesce` must stop the socket before the service, query both `ActiveState` values and the service `MainPID`, and require `inactive`, `inactive`, and `0` respectively.

- [ ] **Step 5: Run inner package tests**

Run:

```bash
go test ./internal/workspace/incus -run 'Test(VerifyNativeBtrfs|WaitReady|Initialize|SyncImages|Quiesce)' -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit inner daemon operations**

```bash
git add internal/workspace/incus/inner.go internal/workspace/incus/inner_test.go
git commit -m "feat: validate and quiesce native nested Incus"
```

---

### Task 5: Build and Refresh the Cold Incus-State Seed

**Files:**
- Create: `internal/workspace/incus/sync.go`
- Create: `internal/workspace/incus/sync_test.go`

**Interfaces:**
- Consumes: Task 1 configuration, Task 2 `GetStoragePool`, Task 3 exclusive seed locking/device helpers, and Task 4 inner operations.
- Produces: `incusworkspace.Sync(context.Context, config.Config, io.Writer, io.Writer) error`.

- [ ] **Step 1: Create a recording lifecycle client and success-path test**

In `sync_test.go`, define a fake implementing the sync client interface and record calls. The success test for a new seed with one image must require this order:

```go
want := []string{
    "resolve-pool",
    "get-pool pool1",
    "get-volume kanedias-incus-seed",
    "create-volume kanedias-incus-seed",
    "init-ca",
    "ensure-network",
    "ensure-profile sandbox",
    "create-instance",
    "start-instance",
    "exec systemctl is-system-running --wait",
    "exec update-ca-certificates",
    "exec getent ahosts images.linuxcontainers.org",
    "exec incus admin waitready --timeout 60",
    "exec incus admin init --minimal",
    "exec incus admin waitready --timeout 60",
    "exec incus query /1.0/storage-pools/default",
    "exec incus image copy images:debian/13 local: --copy-aliases --auto-update --reuse",
    "exec systemctl stop incus.socket",
    "exec systemctl stop incus.service",
    "exec systemctl show --property=ActiveState --value incus.socket",
    "exec systemctl show --property=ActiveState --value incus.service",
    "exec systemctl show --property=MainPID --value incus.service",
    "stop-instance",
    "get-instance",
    "update-instance",
    "delete-instance",
    "disconnect",
}
```

Assert the maintenance instance has the configured base image, root disk, and `incus-state` device at `/var/lib/incus`.

- [ ] **Step 2: Add focused failure and cleanup tests**

Add tests with explicit assertions for:

- outer pool driver `dir` fails before seed lookup or resource creation;
- an existing cold seed skips `create-volume` and skips `incus admin init --minimal`, but still validates and refreshes images;
- an existing seed with a non-empty `UsedBy` list fails before creating the maintenance container;
- a new seed initialization failure deletes the newly created seed after maintenance-container cleanup;
- an existing seed refresh failure preserves the existing seed;
- cancellation during image copy uses `context.WithoutCancel` plus a 30-second cleanup deadline;
- cleanup quiesces nested Incus before stopping the outer maintenance instance;
- failure to quiesce is returned and does not report success;
- an ambiguous submitted `CreateInstance` error marks the instance as owned and triggers maintenance-instance cleanup;
- an ambiguous submitted `StartInstance` error marks the instance as potentially running and triggers a forced stop before cleanup;
- cleanup removes the `incus-state` device before deleting the maintenance instance;
- a cleanup error is joined with the primary error.

- [ ] **Step 3: Run synchronization tests and verify failure**

Run:

```bash
go test ./internal/workspace/incus -run 'TestSync' -count=1
```

Expected: compilation fails because `Sync` and its lifecycle interfaces do not exist.

- [ ] **Step 4: Implement the synchronization dependencies and public entry point**

In `sync.go`, define:

```go
const (
    maintenanceDevice = DeviceName
    cleanupTimeout    = 30 * time.Second
    systemdTimeout    = 60 * time.Second
    dnsTimeout        = 60 * time.Second
)

type client interface {
    VolumeClient
    Disconnect()
    ResolvePool(context.Context, string) (string, error)
    GetStoragePool(context.Context, string) (*api.StoragePool, error)
    CreateStorageVolume(context.Context, string, string) error
    GetNetwork(context.Context, string) (*api.Network, error)
    CreateNetwork(context.Context, api.NetworksPost) error
    EnsureProfile(context.Context, string, []byte) error
    CreateInstance(context.Context, api.InstancesPost) error
    StartInstance(context.Context, string) error
    StopInstance(context.Context, string, bool) error
    GetInstance(context.Context, string) (*api.Instance, string, error)
    UpdateInstance(context.Context, string, api.InstancePut, string) error
    DeleteInstance(context.Context, string) error
    Executor
}
```

`defaultDependencies` must connect through `incusclient.Connect`, initialize the proxy CA, call `network.EnsureWithClient`, render the sandbox profile, generate a name with `fmt.Sprintf("workspace-incus-sync-%d", time.Now().UnixNano())`, and use `incusclient.OperationWasSubmitted`.

Expose:

```go
func Sync(ctx context.Context, cfg config.Config, stdout, stderr io.Writer) error {
    return syncWithDependencies(ctx, cfg, stdout, stderr, defaultDependencies())
}
```

- [ ] **Step 5: Implement the cold-seed lifecycle**

Implement `syncWithDependencies` in the order established by the success test:

1. Validate lifecycle config before connecting.
2. Resolve and fetch the outer pool; return `outer Incus storage pool %q uses %q, want btrfs` unless `Driver == "btrfs"`.
3. Acquire an exclusive lock for `(pool, seed)` and hold it until the seed is cold and maintenance cleanup finishes.
4. Detect/create the seed and track `seedCreated`; for an existing seed, require `len(seed.UsedBy) == 0` before attaching it to the maintenance container.
5. Ensure proxy CA, network, and sandbox profile.
6. Create/start the maintenance container with the seed at `/var/lib/incus`.
7. Wait for systemd, update trusted CAs, and wait for DNS resolution of `images.linuxcontainers.org`.
8. Call `initialize(ctx, incus, name, seedCreated, systemdTimeout)`, `syncImages(ctx, incus, name, cfg.Workspace.Incus.Images)`, and `quiesce(ctx, incus, name)`.
9. Stop the outer maintenance container.
10. Fetch it, remove `DeviceName` from local devices, update it with the ETag, and delete it.

The deferred cleanup must use a non-cancelled bounded context, attempt nested quiescing whenever the outer container reached running state, stop the outer container, detach the seed, and delete the maintenance container. Set `instanceCreated=true` when create succeeds or returns an error recognized by `operationWasSubmitted`; set `instanceRunning=true` when start succeeds or returns a submitted-operation error. Delete the seed only when `seedCreated` is true and the overall operation failed.

- [ ] **Step 6: Run synchronization and package tests**

Run:

```bash
go test ./internal/workspace/incus -count=1
go test -race ./internal/workspace/incus -count=1
```

Expected: PASS with no race reports.

- [ ] **Step 7: Commit seed synchronization**

```bash
git add internal/workspace/incus/sync.go internal/workspace/incus/sync_test.go
git commit -m "feat: synchronize cold nested Incus seed state"
```

---

### Task 6: Expose `workspace repos sync` and `workspace incus sync`

**Files:**
- Modify: `cmd/root.go`
- Modify: `cmd/workspace.go`
- Modify: `cmd/root_test.go`

**Interfaces:**
- Consumes: existing `workspace.Sync` for repositories and Task 5 `incusworkspace.Sync` for Incus state.
- Produces: `services.syncRepos` and `services.syncIncusWorkspace` function fields.
- Produces only these leaf commands: `workspace repos sync` and `workspace incus sync`.

- [ ] **Step 1: Change command hierarchy expectations first**

Update `TestCommandHierarchyAndFlags`:

```go
assertChildCommands(t, mustFindCommand(t, root, "workspace"), "incus", "repos")
assertChildCommands(t, mustFindCommand(t, root, "workspace", "incus"), "sync")
assertChildCommands(t, mustFindCommand(t, root, "workspace", "repos"), "sync")
```

Replace every `{"workspace", "sync"}` path with both:

```go
{"workspace", "incus", "sync"},
{"workspace", "repos", "sync"},
```

Add an assertion that `root.Find([]string{"workspace", "sync"})` does not resolve to an executable sync command.

In lifecycle delegation tests, add:

```go
{name: "workspace repos sync", args: []string{"workspace", "repos", "sync"}, workflow: "workspace-repos"},
{name: "workspace incus sync", args: []string{"workspace", "incus", "sync"}, workflow: "workspace-incus"},
```

Add both paths to config-failure and extra-argument test tables.

- [ ] **Step 2: Run command tests and verify failure**

Run:

```bash
go test ./cmd -run 'Test(CommandHierarchyAndFlags|LifecycleCommandDelegation|LifecycleCommandsStopWhenConfigLoadFails|LifecycleCommandsRejectExtraArguments)$' -count=1
```

Expected: FAIL because the old direct `workspace sync` hierarchy remains.

- [ ] **Step 3: Split service wiring without compatibility aliases**

In `cmd/root.go`, replace:

```go
syncWorkspace func(context.Context, config.Config, io.Writer, io.Writer) error
```

with:

```go
syncRepos          func(context.Context, config.Config, io.Writer, io.Writer) error
syncIncusWorkspace func(context.Context, config.Config, io.Writer, io.Writer) error
```

Wire `workspace.Sync` to `syncRepos` and `incusworkspace.Sync` to `syncIncusWorkspace` in `realServices`. Import the new package as:

```go
incusworkspace "github.com/sklarsa/kanedias/internal/workspace/incus"
```

Update every test stub and dependency-rejection helper to initialize both fields.

- [ ] **Step 4: Build the nested Cobra hierarchy**

Replace `cmd/workspace.go` with constructors for:

```text
workspace
├── incus
│   └── sync
└── repos
    └── sync
```

Both leaf commands use `cobra.NoArgs`, load the configured file, and pass the command context and exact stdout/stderr writers to their corresponding service. Do not register a direct `sync` child under `workspace`.

- [ ] **Step 5: Run command and full unit tests**

Run:

```bash
go test ./cmd -count=1
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 6: Commit the command hierarchy**

```bash
git add cmd/root.go cmd/workspace.go cmd/root_test.go
git commit -m "feat: split workspace synchronization commands"
```

---

### Task 7: Attach Private Incus State to Manual Sandboxes

**Files:**
- Modify: `internal/sandbox/sandbox.go`
- Modify: `internal/sandbox/sandbox_test.go`

**Interfaces:**
- Consumes: `incusworkspace.SeedVolume`, `SandboxVolume`, `Clone`, `Device`, `WaitReady`, `VerifyNativeBtrfs`, and `Delete`.
- Preserves: existing repository clone at `/workspace` and existing sandbox naming/locking behavior.
- Produces: `incusworkspace.SandboxVolume(name)` (`"kanedias-incus-" + name`) attached as `incus-state` at `/var/lib/incus`.

- [ ] **Step 1: Update the successful create test for two volumes**

Change `TestCreateOrdersLifecycleAndBuildsOwnedWorkspaceDevice` to require:

```go
wantCalls := []string{
    "resolve-pool", "lock", "init-ca", "ensure-network", "ensure-profile sandbox",
    "get-volume kanedias-workspace-seed",
    "copy-volume kanedias-workspace-seed kanedias-workspace-demo",
    "clone-incus kanedias-incus-seed kanedias-incus-demo",
    "create-instance", "start",
    "exec systemctl is-system-running --wait",
    "exec update-ca-certificates",
    "exec incus admin waitready --timeout 60",
    "exec incus query /1.0/storage-pools/default",
}
```

Assert:

```go
wantIncusDevice := map[string]string{
    "type": "disk", "pool": "pool1", "source": "kanedias-incus-demo", "path": "/var/lib/incus",
}
if got := request.Devices[incusworkspace.DeviceName]; !equalStringMap(got, wantIncusDevice) {
    t.Fatalf("Incus-state device = %#v, want %#v", got, wantIncusDevice)
}
```

Extend test dependencies with injectable wrappers that record `clone-incus`, call Task 4 verification helpers against the fake executor, and return `CloneResult{Created: true}`.

- [ ] **Step 2: Add create rollback tests for Incus state**

Add tests proving:

- repository copy failure does not attempt an Incus-state clone;
- Incus-state clone failure deletes an ambiguous clone when `CloneResult.Created` is true and deletes the already-created repository clone;
- a pre-submission instance creation failure deletes both owned clone volumes but not the colliding instance;
- an ambiguous submitted instance creation failure treats the deterministic instance as owned and attempts deletion;
- an ambiguous submitted start failure treats the instance as potentially running and attempts a forced stop before deletion;
- nested readiness failure stops/deletes the created outer instance and then deletes both clone volumes;
- request cancellation cleanup uses a non-cancelled 30-second context for the instance and both volumes;
- cleanup order is `stop`, `delete-instance`, `delete-volume kanedias-incus-demo`, then `delete-volume kanedias-workspace-demo`.

- [ ] **Step 3: Add dual-device destroy ownership tests**

Update the verified instance fixture to contain:

```go
Devices: api.DevicesMap{
    "workspace": {
        "type": "disk", "source": "kanedias-workspace-demo", "path": "/workspace",
    },
    incusworkspace.DeviceName: {
        "type": "disk", "source": "kanedias-incus-demo", "path": "/var/lib/incus",
    },
}
```

Require successful destroy to delete both volumes after the instance. Add table rows for missing and mismatched Incus-state devices and assert no instance or volume deletion occurs. Add an orphan cleanup test where the instance is absent and both deterministic clone volumes are removed. Extend the protected-seed test to cover `kanedias-incus-seed`.

- [ ] **Step 4: Run sandbox tests and verify failure**

Run:

```bash
go test ./internal/sandbox -count=1
```

Expected: FAIL because sandbox lifecycle only knows the repository volume.

- [ ] **Step 5: Integrate the Incus-state clone into create**

In `sandbox.go`:

1. Add `incusVolumeCreated` and the deterministic clone name to create state.
2. After the repository copy, call the injected/default `incusworkspace.Clone` with the resolved pool, configured seed, and sandbox name.
3. Preserve `CloneResult.Created` even when clone returns an error.
4. Add `incusworkspace.DeviceName: incusworkspace.Device(pool, clone.Name)` to the instance request.
5. After outer systemd and CA readiness, call `incusworkspace.WaitReady` and `incusworkspace.VerifyNativeBtrfs`.
6. In deferred rollback, delete the instance first, then the Incus-state clone, then the repository clone.

Add these dependency fields so tests do not acquire real seed locks:

```go
cloneIncusState func(context.Context, incusworkspace.VolumeClient, string, string, string) (incusworkspace.CloneResult, error)
waitNestedIncus func(context.Context, incusworkspace.Executor, string, time.Duration) error
verifyNestedIncus func(context.Context, incusworkspace.Executor, string) error
```

Add `operationWasSubmitted func(error) bool` to dependencies and default it to `incusclient.OperationWasSubmitted`. Set instance ownership/running flags on submitted create/start errors before returning. Default the three nested-Incus function fields to the Task 3 and Task 4 functions.

- [ ] **Step 6: Integrate verified deletion of both volumes**

In `destroy`:

1. Compute both deterministic clone names and reject either if it equals its configured seed.
2. For an existing instance, verify both local device sources before stopping or deleting anything.
3. Delete the instance.
4. Independently query and delete the Incus-state clone and repository clone, treating not-found as success.
5. Join deletion errors so failure to delete one clone does not prevent attempting the other.

- [ ] **Step 7: Run sandbox and full tests**

Run:

```bash
go test ./internal/sandbox -count=1
go test -race ./internal/sandbox -count=1
go test ./... -count=1
```

Expected: PASS with no race reports.

Verify prohibited files remain unchanged relative to the design commit:

```bash
git diff b3d054d -- internal/session cmd/session.go docs/architecture/session-supervisor.md
```

Expected: no output.

- [ ] **Step 8: Commit manual sandbox integration**

```bash
git add internal/sandbox/sandbox.go internal/sandbox/sandbox_test.go
git commit -m "feat: clone nested Incus state into sandboxes"
```

---

### Task 8: Add the Opt-In Native-Btrfs Isolation Test and Final Verification

**Files:**
- Create: `internal/workspace/incus/live_incus_test.go`
- Modify: `docs/superpowers/specs/2026-08-07-native-nested-incus-workspace-design.md` only if implementation discovered a concrete command or API correction; do not broaden scope.

**Interfaces:**
- Consumes: `incusworkspace.Sync`, `sandbox.Create`, `sandbox.Destroy`, and `incusclient.Connect`.
- Produces: environment gate `KANEDIAS_LIVE_NESTED_INCUS=1` and optional `KANEDIAS_CONFIG` path.

- [ ] **Step 1: Write the skipped-by-default live test**

Create `internal/workspace/incus/live_incus_test.go` with `//go:build incus`, `package incusworkspace_test`, and a test named `TestLiveNativeNestedIncusIsolation` so importing `internal/sandbox` does not create a test import cycle. The test must:

```go
if os.Getenv("KANEDIAS_LIVE_NESTED_INCUS") != "1" {
    t.Skip("set KANEDIAS_LIVE_NESTED_INCUS=1 to run the native nested Incus test")
}
```

Load `KANEDIAS_CONFIG` or `./config.toml`, connect to outer Incus, resolve the pool, call `outer.GetStoragePool(ctx, pool)`, and skip with a clear message unless the returned `api.StoragePool.Driver == "btrfs"`.

Generate a suffix from the current nanosecond timestamp. Override configuration with unique names:

```go
cfg.Workspace.Volume = "kanedias-live-repos-" + suffix
cfg.Workspace.Repos = nil
cfg.Workspace.Incus.Volume = "kanedias-live-incus-seed-" + suffix
cfg.Workspace.Incus.Images = []string{"images:debian/13"}
```

Create the empty repository seed with `CreateStorageVolume`, call `incusworkspace.Sync`, set `sandboxA := "kanedias-live-a-" + suffix` and `sandboxB := "kanedias-live-b-" + suffix`, then start two goroutines calling `sandbox.Create` for those names. Collect both errors and fail if either is non-nil.

- [ ] **Step 2: Add nested isolation assertions and unconditional cleanup**

In `sandboxA`, execute:

```text
incus query /1.0/storage-pools/default
incus image list --format csv -c l
incus launch debian/13 inner-a
incus exec inner-a -- sh -c 'printf sandbox-a >/root/kanedias-marker'
incus exec inner-a -- cat /root/kanedias-marker
incus list inner-b --format csv -c n
```

In `sandboxB`, execute:

```text
incus query /1.0/storage-pools/default
incus image list --format csv -c l
incus launch debian/13 inner-b
incus exec inner-b -- sh -c 'printf sandbox-b >/root/kanedias-marker'
incus exec inner-b -- cat /root/kanedias-marker
incus list inner-a --format csv -c n
```

Require marker reads to return `sandbox-a` and `sandbox-b` respectively. Require each final `incus list` command to return an empty name column.

Register cleanup immediately after every successful resource creation. Cleanup must attempt, in order:

1. delete inner instances with `incus delete --force`;
2. call `sandbox.Destroy` for both outer sandboxes;
3. delete the unique Incus seed;
4. delete the unique repository seed.

Join cleanup errors and report them from `t.Cleanup`.

- [ ] **Step 3: Compile the build-tagged test without executing live operations**

Run:

```bash
go test -tags=incus ./internal/workspace/incus -run TestLive -count=1
```

Expected: PASS with the live test skipped because `KANEDIAS_LIVE_NESTED_INCUS` is unset.

- [ ] **Step 4: Run all non-live verification**

Run:

```bash
gofmt -w $(find cmd internal -type f -name '*.go' -print)
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
shellcheck internal/image/install.sh
git diff --check
```

Expected: every command exits successfully and `git diff --check` prints nothing.

Confirm the forbidden integration layer is untouched:

```bash
git diff b3d054d -- internal/session cmd/session.go docs/architecture/session-supervisor.md
```

Expected: no output.

- [ ] **Step 5: Run the live PoC when the host is ready**

Run:

```bash
KANEDIAS_LIVE_NESTED_INCUS=1 \
KANEDIAS_CONFIG="$PWD/config.toml" \
go test -tags=incus ./internal/workspace/incus -run TestLiveNativeNestedIncusIsolation -v -count=1 -timeout=20m
```

Expected: both outer sandboxes start, both nested pools report native Btrfs, both inner containers launch from the cached image, isolation assertions pass, and cleanup removes all uniquely named resources.

If the environment cannot run the live test, record the exact missing prerequisite in the final delivery instead of claiming live verification.

- [ ] **Step 6: Commit the live PoC test**

```bash
git add internal/workspace/incus/live_incus_test.go docs/superpowers/specs/2026-08-07-native-nested-incus-workspace-design.md
git commit -m "test: prove nested Incus seed clone isolation"
```

- [ ] **Step 7: Record final evidence for review**

Capture:

```bash
git status --short
git log --oneline b3d054d..HEAD
git diff --stat b3d054d..HEAD
incus storage list
```

The delivery report must list changed files, all verification commands and outcomes, whether the live test ran, and the deferred risks: nested-Btrfs quota escape, mutable seed updates, and duplicated outer/inner image storage.
