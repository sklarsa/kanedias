# Session Names and Repository Start Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add optional in-memory root display names and GUI renaming, allow an optional configured repository to become the inherited Pi working directory, and make `/workspace` writable by `kanedias`.

**Architecture:** The manager owns display-only names and browser launch validation. A small immutable `config.WorkspaceStart` value carries an allowlisted repository slug and checkout basename through root and child bootstraps into provisioning; provisioners repair workspace ownership, validate the checkout before Pi starts, and set a launcher working-directory environment value. A bounded root startup-status pipe carries only an allowlisted error code back to the manager so the server can show specific missing-repository copy without exposing internal errors.

**Tech Stack:** Go 1.26.5, Cobra, Incus Go API, Chi, Datastar, Go `html/template`, browser JavaScript tested with Node's built-in test runner, Bash launch scripts.

## Global Constraints

- Display names are optional, trimmed, limited to 80 Unicode code points, reject Unicode control characters, may be duplicated, and never replace immutable session IDs.
- Clearing a display name restores the immutable root session ID as the GUI fallback.
- Display names remain in manager memory only; persistence is deferred to GitHub issue #57.
- Repository selection is optional and accepts only exact configured `workspace.repos` slugs; arbitrary URLs, paths, branches, commits, and launch-time clones remain forbidden.
- An empty repository selection starts Pi in `/workspace`.
- A selected repository starts root, fresh descendants, forked descendants, and nested descendants in `/workspace/repos/<checkout>`.
- A selected checkout that is missing, symlinked, escaped, or not its own Git top level fails before Pi task execution and produces the fixed modal copy `The selected repository is not present in the workspace.`
- Browser-supplied values never become shell fragments or absolute paths; in-instance checks use argument-array execution.
- `/workspace` and `/workspace/repos` must be `kanedias:kanedias 0755`; root provisioning repairs old seed clones and workspace sync repairs the durable seed.
- Invalid launch input must be rejected before token, path, log, pipe, process, Incus volume, or Incus instance side effects.
- Root startup status is strict, bounded, private inherited data and carries an allowlisted status/error code only—never internal paths or raw errors.
- Existing running sessions are not mutated.
- Do not run destructive live Incus acceptance without the repository's existing explicit authorization.

## File and Interface Map

### New focused files

- `internal/config/workspace_start.go` — canonical configured-repository parsing and immutable `WorkspaceStart` validation/path derivation.
- `internal/config/workspace_start_test.go` — repository parsing, duplicate destination, and workspace-start validation tests.
- `internal/supervisor/process/root_status.go` — bounded root startup ready/failure wire record.
- `internal/supervisor/process/root_status_test.go` — strict codec, allowlisted-code, and short-write tests.
- `internal/supervisor/provision/workspace.go` — shared root/child in-instance ownership repair and repository validation.
- `internal/supervisor/provision/workspace_test.go` — argument-array command order and typed failure tests.
- `internal/manager/names_test.go` — in-memory root name projection, rename, fallback, and revision tests.

### Existing files grouped by responsibility

- Launch catalog: `internal/manager/launch.go`, `internal/manager/launch_test.go`.
- Root spawn/admission: `internal/manager/spawn.go`, `internal/manager/spawn_test.go`.
- Manager root state: `internal/manager/discovery.go`, `internal/manager/manager.go`, `internal/manager/types.go`, `internal/manager/monitor.go`.
- Root/child process contracts: `internal/supervisor/process/root_bootstrap.go`, `internal/supervisor/process/protocol.go`, `internal/supervisor/process/process_test.go`.
- Supervisor threading: `internal/supervisor/node.go`, `internal/supervisor/node_test.go`, `cmd/session.go`, `cmd/session_runtime.go`, `cmd/session_test.go`, `cmd/session_runtime_test.go`.
- Provisioning: `internal/supervisor/provision/types.go`, `root.go`, `root_test.go`, `child.go`, `child_test.go`.
- Workspace seed: `internal/workspace/workspace.go`, `repositories.go`, `workspace_test.go`.
- Image runtime: `internal/image/kanedias-pi-env`, `internal/image/kanedias-pi-rpc`, `internal/image/image_test.go`.
- Server API/view: `internal/server/server.go`, `handler.go`, `actions.go`, `signals.go`, `view.go`, `actions_test.go`, `handler_test.go`.
- Browser: `internal/server/web/session-modal.html`, `session-modal.js`, `session-modal.test.js`, `fleet.html`, `detail.html`, `app.css`.

---

### Task 1: Canonical Workspace Repository and Start Types

**Files:**
- Create: `internal/config/workspace_start.go`
- Create: `internal/config/workspace_start_test.go`
- Modify: `internal/workspace/repositories.go`
- Test: `internal/workspace/workspace_test.go`

**Interfaces:**
- Produces: `config.ParseWorkspaceRepositories([]string) ([]config.WorkspaceRepository, error)`.
- Produces: `config.WorkspaceRepository{Slug string, Checkout string}`.
- Produces: `config.WorkspaceStart{Repository string, Checkout string}`, `Validate() error`, and `Directory() string`.
- Consumed by: manager launch resolution, root/child bootstraps, provisioners, workspace sync.

- [ ] **Step 1: Write failing config tests for repository parsing and immutable starts**

Create table-driven tests covering valid sorted input, malformed slugs, whitespace/control characters, `.`/`..` checkout names, duplicate checkout basenames, default `/workspace`, selected `/workspace/repos/repo`, mismatched checkout, and path-like checkout values:

```go
func TestParseWorkspaceRepositories(t *testing.T) {
    got, err := ParseWorkspaceRepositories([]string{"two/beta", "one/alpha"})
    if err != nil { t.Fatal(err) }
    want := []WorkspaceRepository{
        {Slug: "one/alpha", Checkout: "alpha"},
        {Slug: "two/beta", Checkout: "beta"},
    }
    if !reflect.DeepEqual(got, want) { t.Fatalf("got %#v, want %#v", got, want) }
}

func TestWorkspaceStartValidationAndDirectory(t *testing.T) {
    tests := []struct {
        name string
        start WorkspaceStart
        wantDir string
        wantErr bool
    }{
        {name: "workspace default", wantDir: "/workspace"},
        {name: "configured checkout", start: WorkspaceStart{Repository: "owner/repo", Checkout: "repo"}, wantDir: "/workspace/repos/repo"},
        {name: "mismatched checkout", start: WorkspaceStart{Repository: "owner/repo", Checkout: "other"}, wantErr: true},
        {name: "browser path forbidden", start: WorkspaceStart{Repository: "owner/repo", Checkout: "../repo"}, wantErr: true},
    }
    // Validate each case and assert Directory only for valid values.
}
```

- [ ] **Step 2: Run the new config tests and verify they fail**

Run: `go test ./internal/config -run 'Test(ParseWorkspaceRepositories|WorkspaceStart)' -count=1`

Expected: FAIL because `WorkspaceRepository`, `WorkspaceStart`, and parsing do not exist.

- [ ] **Step 3: Implement the canonical parser and start value**

Implement a focused file with constants and strict structured validation:

```go
const (
    WorkspaceRoot = "/workspace"
    WorkspaceRepositoriesRoot = "/workspace/repos"
)

type WorkspaceRepository struct {
    Slug     string
    Checkout string
}

type WorkspaceStart struct {
    Repository string `json:"repository,omitempty"`
    Checkout   string `json:"checkout,omitempty"`
}

func ParseWorkspaceRepositories(slugs []string) ([]WorkspaceRepository, error) {
    // Require exactly owner/repository, safe non-control/non-space components,
    // repository != "."/"..", and unique checkout basenames. Return slug-sorted copies.
}

func (start WorkspaceStart) Validate() error {
    if start.Repository == "" && start.Checkout == "" { return nil }
    repos, err := ParseWorkspaceRepositories([]string{start.Repository})
    if err != nil { return err }
    if len(repos) != 1 || repos[0].Checkout != start.Checkout {
        return fmt.Errorf("workspace start checkout does not match repository")
    }
    return nil
}

func (start WorkspaceStart) Directory() string {
    if start.Repository == "" { return WorkspaceRoot }
    return filepath.Join(WorkspaceRepositoriesRoot, start.Checkout)
}
```

Use a component regexp compatible with configured GitHub owner/repository names (`[A-Za-z0-9_.-]+`) plus explicit dot-segment rejection. Do not accept slash, backslash, whitespace, or control characters inside components.

- [ ] **Step 4: Refactor workspace repository parsing to consume the canonical parser**

Keep the workspace-private URL field while deleting the duplicate slug/destination validation:

```go
func parseRepositories(slugs []string) ([]repository, error) {
    configured, err := config.ParseWorkspaceRepositories(slugs)
    if err != nil { return nil, err }
    repositories := make([]repository, 0, len(configured))
    for _, item := range configured {
        repositories = append(repositories, repository{
            slug: item.Slug,
            name: item.Checkout,
            url: "https://github.com/" + item.Slug + ".git",
        })
    }
    return repositories, nil
}
```

- [ ] **Step 5: Run config and workspace parsing tests**

Run: `go test ./internal/config ./internal/workspace -run 'Test(ParseWorkspaceRepositories|WorkspaceStart|SyncValidatesRepositories)' -count=1`

Expected: PASS.

- [ ] **Step 6: Commit the canonical types**

```bash
git add internal/config/workspace_start.go internal/config/workspace_start_test.go internal/workspace/repositories.go internal/workspace/workspace_test.go
git commit -m "feat: define workspace repository starts"
```

---

### Task 2: Repair Durable and Cloned Workspace Ownership

**Files:**
- Modify: `internal/workspace/workspace.go`
- Modify: `internal/workspace/repositories.go`
- Modify: `internal/workspace/workspace_test.go`
- Create: `internal/supervisor/provision/workspace.go`
- Create: `internal/supervisor/provision/workspace_test.go`
- Modify: `internal/supervisor/provision/root.go`
- Modify: `internal/supervisor/provision/root_test.go`

**Interfaces:**
- Produces: workspace-private `prepareWorkspaceRoot(...)` for seed preparation.
- Produces: provision-private `prepareSessionWorkspace(context.Context, instanceExecClient, string, config.WorkspaceStart) error` (repository validation is completed in Task 5; default-start ownership repair is implemented now).
- Extends: root provisioning's Incus client seam with argument-array `Exec`.

- [ ] **Step 1: Write failing workspace-sync tests for exact ownership repair**

Update `TestSyncEmptyRepositoriesEnsuresSeedAndWarns` so even an empty repo list expects an ephemeral instance lifecycle and ownership commands before warning/cleanup. Extend the ordered assertion in `TestSyncRetriesSystemdBeforeCAAndDNSAndDestructiveRefresh` with:

```text
exec chown kanedias:kanedias /workspace
exec chmod 0755 /workspace
exec install -d -o kanedias -g kanedias -m 0755 /workspace/repos
exec chown kanedias:kanedias /workspace/repos
exec chmod 0755 /workspace/repos
```

Assert these occur before `syncRepositories`, and that empty config skips CA/DNS/Git operations only after ownership is repaired.

- [ ] **Step 2: Run workspace tests and verify they fail**

Run: `go test ./internal/workspace -run 'TestSync(EmptyRepositories|RetriesSystemd)' -count=1`

Expected: FAIL because empty configuration returns before mounting the seed and `/workspace` is never repaired.

- [ ] **Step 3: Implement durable seed ownership repair**

Replace `prepareRepositoryRoot` with `prepareWorkspaceRoot` that first rejects a symlinked `/workspace/repos`, then uses separate root argument arrays to enforce ownership and mode even on existing directories:

```go
commands := [][]string{
    {"chown", managedUser + ":" + managedUser, workspacePath},
    {"chmod", "0755", workspacePath},
    {"install", "-d", "-o", managedUser, "-g", managedUser, "-m", "0755", workspacePath + "/repos"},
    {"chown", managedUser + ":" + managedUser, workspacePath + "/repos"},
    {"chmod", "0755", workspacePath + "/repos"},
}
```

Do not rely on `install -d -m` alone to repair an existing directory. Move the empty-repository warning/return until after the seed is mounted, the instance is ready, and `prepareWorkspaceRoot` succeeds. The empty case still initializes the proxy CA, network, and sandbox profile required to mount the seed; it skips only the in-instance CA update, DNS wait, and Git setup after ownership repair. Retain cleanup.

- [ ] **Step 4: Run workspace tests and verify they pass**

Run: `go test ./internal/workspace -count=1`

Expected: PASS.

- [ ] **Step 5: Write failing root-provisioning tests for defensive clone repair**

Add `Exec` recording to `recordingRootClient`. Assert `ProvisionRoot` executes the same five ownership commands after `start-instance` and before RPC address completion. Add a failure case where `Exec` returns a sentinel and assert cleanup still stops/deletes the owned instance before deleting the volume.

```go
func TestRootProvisionerRepairsClonedWorkspaceBeforeReadiness(t *testing.T) {
    client := &recordingRootClient{}
    _, err := newRootProvisioner(rootTestConfig(), testRootDependencies(client)).ProvisionRoot(context.Background(), validRootRequest())
    if err != nil { t.Fatal(err) }
    assertOrderedCalls(t, client.calls, "start-instance", "exec chown kanedias:kanedias /workspace", "get-state")
}
```

- [ ] **Step 6: Run root provisioning tests and verify they fail**

Run: `go test ./internal/supervisor/provision -run 'TestRootProvisioner(RepairsClonedWorkspace|CleansWorkspacePreparationFailure)' -count=1`

Expected: FAIL because `rootClient` has no `Exec` and provisioning performs no repair.

- [ ] **Step 7: Add the shared provisioner ownership helper and call it from roots**

Define the narrow exec interface:

```go
type instanceExecClient interface {
    Exec(context.Context, string, incusclient.ExecRequest) (string, string, error)
}
```

Implement `prepareSessionWorkspace` with the same exact ownership commands for a default `WorkspaceStart{}`. Add `Exec` to `rootClient`, invoke the helper immediately after successful `StartInstance`, and preserve the existing deferred cleanup/error joining.

- [ ] **Step 8: Run workspace and root provisioning tests**

Run: `go test ./internal/workspace ./internal/supervisor/provision -count=1`

Expected: PASS.

- [ ] **Step 9: Commit ownership repair**

```bash
git add internal/workspace internal/supervisor/provision/workspace.go internal/supervisor/provision/workspace_test.go internal/supervisor/provision/root.go internal/supervisor/provision/root_test.go
git commit -m "fix: make session workspaces writable"
```

---

### Task 3: Validate and Resolve Name and Repository Launch Input

**Files:**
- Modify: `internal/manager/launch.go`
- Modify: `internal/manager/launch_test.go`
- Modify: `internal/manager/spawn.go`
- Modify: `internal/manager/spawn_test.go`

**Interfaces:**
- Produces: `manager.ResolvedSessionLaunch{Name string, Workspace config.WorkspaceStart, Policy config.SessionModelPolicy}`.
- Changes: `LaunchConfiguration.Resolve(SessionLaunchRequest) (ResolvedSessionLaunch, error)`.
- Extends: `SessionLaunchRequest` with `Name` and `Repository`; `SessionLaunchOptions` with repository options.
- Consumed by: spawn/bootstrap, manager naming, server modal.

- [ ] **Step 1: Write failing launch-catalog tests**

Extend fixtures with configured repos and add tests for deterministic copied options, empty/default resolution, exact selected resolution, unknown repo rejection, name trim, duplicate-name acceptance, 81-code-point rejection, and control-character rejection:

```go
func TestLaunchConfigurationResolvesNameRepositoryAndPolicy(t *testing.T) {
    request := launch.DefaultRequest()
    request.Name = "  release triage  "
    request.Repository = "owner/repo"
    got, err := launch.Resolve(request)
    if err != nil { t.Fatal(err) }
    if got.Name != "release triage" { t.Fatalf("name = %q", got.Name) }
    if got.Workspace != (config.WorkspaceStart{Repository: "owner/repo", Checkout: "repo"}) {
        t.Fatalf("workspace = %#v", got.Workspace)
    }
}
```

Assert repository options contain slugs only—no URL or absolute path.

- [ ] **Step 2: Run manager launch tests and verify they fail**

Run: `go test ./internal/manager -run 'TestLaunchConfiguration.*(Name|Repository|Options)' -count=1`

Expected: FAIL because launch request/options/resolution do not contain these fields.

- [ ] **Step 3: Implement resolved launch state and validation**

Add:

```go
type SessionLaunchRequest struct {
    Name       string                 `json:"name"`
    Repository string                 `json:"repository"`
    Root       ModelSelection         `json:"root"`
    Workers    []WorkerModelSelection `json:"workers"`
}

type RepositoryLaunchOption struct { Slug string `json:"slug"` }

type ResolvedSessionLaunch struct {
    Name      string
    Workspace config.WorkspaceStart
    Policy    config.SessionModelPolicy
}
```

Have `NewLaunchConfiguration` call `config.ParseWorkspaceRepositories`, store copied sorted options plus a slug→`WorkspaceStart` map, and reject invalid configured repositories at server construction. `Resolve` must normalize/validate the name and resolve the exact slug before model policy assembly. Use `utf8.RuneCountInString` and `unicode.IsControl`; return `contract.ErrorInvalidRequest` for every input failure.

- [ ] **Step 4: Update spawn to consume resolved launch state before side effects**

At the top of `SpawnRootWithRequest`:

```go
resolved, err := m.launch.Resolve(request)
if err != nil { return "", err }
bootstrap := process.RootBootstrap{Policy: resolved.Policy}
```

Store `resolved.Name` on `pendingRoot` for Task 7. Task 4 adds `Workspace` to the bootstrap contract and then encodes `resolved.Workspace`; this task keeps the current bootstrap shape so its commit remains buildable. Update existing tests that previously expected `Resolve` to return only a model policy to use `.Policy`.

- [ ] **Step 5: Prove invalid name/repository input has no side effects**

Extend `TestSpawnRootWithRequestValidatesBeforeSideEffects` with invalid name and unknown repository subtests and retain assertions for zero token, pipe, starter, and log calls.

- [ ] **Step 6: Run manager tests**

Run: `go test ./internal/manager -count=1`

Expected: PASS.

- [ ] **Step 7: Commit launch validation**

```bash
git add internal/manager/launch.go internal/manager/launch_test.go internal/manager/spawn.go internal/manager/spawn_test.go
git commit -m "feat: resolve session names and repositories"
```

---

### Task 4: Carry the Repository Start Through Root and Child Bootstraps

**Files:**
- Modify: `internal/supervisor/process/root_bootstrap.go`
- Modify: `internal/supervisor/process/root_bootstrap_test.go`
- Modify: `internal/supervisor/process/protocol.go`
- Modify: `internal/supervisor/process/process_test.go`
- Modify: `internal/supervisor/node.go`
- Modify: `internal/supervisor/node_test.go`
- Modify: `internal/supervisor/provision/types.go`
- Modify: `internal/manager/spawn.go`
- Modify: `internal/manager/spawn_test.go`
- Modify: `cmd/session.go`
- Modify: `cmd/session_runtime.go`
- Modify: `cmd/session_test.go`
- Modify: `cmd/session_runtime_test.go`

**Interfaces:**
- Extends: `process.RootBootstrap.Workspace config.WorkspaceStart`.
- Extends: `process.Bootstrap.Workspace config.WorkspaceStart`.
- Extends: `supervisor.Dependencies.Workspace config.WorkspaceStart`.
- Extends: `provision.RootRequest.Workspace` and `provision.ChildRequest.Workspace`.
- Preserves: zero `WorkspaceStart` means `/workspace` for direct CLI roots.

- [ ] **Step 1: Write failing strict bootstrap tests**

Add valid selected workspace values to root/child fixtures. Assert exact round trips, default zero acceptance, invalid mismatch/path rejection, unknown-field rejection, and caller/decoded value independence:

```go
bootstrap.Workspace = config.WorkspaceStart{Repository: "owner/repo", Checkout: "repo"}
```

For child tests, mutate a copied bootstrap after spawn and assert the child's stored `Workspace` remains unchanged.

- [ ] **Step 2: Run process tests and verify they fail**

Run: `go test ./internal/supervisor/process -run 'Test(RootBootstrap|Bootstrap).*' -count=1`

Expected: FAIL because the bootstrap structs lack `Workspace`.

- [ ] **Step 3: Extend strict root and child bootstrap contracts**

Add a `Workspace config.WorkspaceStart` field with JSON name `workspace` to both structs. Call `Workspace.Validate()` in `EncodeRootBootstrap`, `DecodeRootBootstrap`, and `Bootstrap.Validate`. Preserve the existing 1 MiB bound and unknown-field behavior. Update `Manager.SpawnRootWithRequest` to encode `resolved.Workspace` into `RootBootstrap.Workspace` now that the field exists.

- [ ] **Step 4: Write failing supervisor/runtime inheritance tests**

In `node_test.go`, capture the `process.Bootstrap` passed to `SpawnChild` and assert it exactly equals `node.deps.Workspace`. In `session_runtime_test.go`, assert root options reach `Dependencies`, child `bootstrap.Workspace` reaches `ChildRequest.Workspace`, and a grandchild receives the same value after config defaults are changed.

- [ ] **Step 5: Run supervisor/cmd tests and verify they fail**

Run: `go test ./internal/supervisor ./cmd -run 'Test.*Workspace(Start|Repository|Inheritance)' -count=1`

Expected: FAIL because runtime and provisioning request types do not carry the value.

- [ ] **Step 6: Thread one immutable value through runtime boundaries**

Add `Workspace config.WorkspaceStart` to `SessionOptions` and `supervisor.Dependencies`. Decode `RootBootstrap.Workspace` in `cmd/session.go`; direct CLI roots use `config.WorkspaceStart{}`. In `runSupervisor`, validate once and pass it into root `Dependencies`. In `Node.Start`, copy it into `provision.RootRequest`; in `Node.CreateChild`, copy it into `process.Bootstrap`. In child runtime, validate `bootstrap.Workspace`, pass it into child `Dependencies`, and into `provision.ChildRequest`.

Do not re-read `cfg.Workspace.Repos` to select a different start after launch.

- [ ] **Step 7: Run focused and package tests**

Run: `go test ./internal/supervisor/process ./internal/supervisor ./cmd -count=1`

Expected: PASS.

- [ ] **Step 8: Commit bootstrap inheritance**

```bash
git add internal/supervisor/process/root_bootstrap.go internal/supervisor/process/root_bootstrap_test.go internal/supervisor/process/protocol.go internal/supervisor/process/process_test.go internal/supervisor/node.go internal/supervisor/node_test.go internal/supervisor/provision/types.go internal/manager/spawn.go internal/manager/spawn_test.go cmd/session.go cmd/session_runtime.go cmd/session_test.go cmd/session_runtime_test.go
git commit -m "feat: inherit session repository starts"
```

---

### Task 5: Validate Checkouts and Start Pi in the Selected Repository

**Files:**
- Modify: `internal/supervisor/contract/errors.go`
- Modify: `internal/supervisor/contract/types_test.go`
- Modify: `internal/supervisor/provision/workspace.go`
- Modify: `internal/supervisor/provision/workspace_test.go`
- Modify: `internal/supervisor/provision/root.go`
- Modify: `internal/supervisor/provision/root_test.go`
- Modify: `internal/supervisor/provision/child.go`
- Modify: `internal/supervisor/provision/child_test.go`
- Modify: `internal/image/kanedias-pi-env`
- Modify: `internal/image/kanedias-pi-rpc`
- Modify: `internal/image/image_test.go`

**Interfaces:**
- Produces: `contract.ErrorWorkspaceRepositoryUnavailable`.
- Extends: root/child Incus config with `environment.KANEDIAS_PI_WORKDIR`.
- Consumes: `RootRequest.Workspace` and `ChildRequest.Workspace`.
- Preserves: launcher defaults to `/workspace` when the environment value is absent.

- [ ] **Step 1: Write failing provisioner workspace-validation tests**

Create an exec fake that returns controlled stdout/stderr/errors per argument array. Cover:

- default start performs ownership repair only;
- selected path missing;
- path is a symlink;
- canonical path differs;
- `.git` is missing/symlinked;
- `git rev-parse --show-toplevel` differs;
- valid checkout succeeds;
- every invalid checkout returns `ErrorWorkspaceRepositoryUnavailable` while preserving the underlying error for logs.

Expected argument arrays include `test`, `realpath`, and managed-user Git invocation—never `sh -c`.

- [ ] **Step 2: Run workspace provision tests and verify they fail**

Run: `go test ./internal/supervisor/provision -run 'TestPrepareSessionWorkspace' -count=1`

Expected: FAIL because selected-checkout validation and the error code do not exist.

- [ ] **Step 3: Implement typed checkout validation**

Add the contract code and map it to service-unavailable semantics internally. Extend `prepareSessionWorkspace` after ownership repair:

```go
if start.Repository == "" { return nil }
expected := start.Directory()
checks := [][]string{
    {"test", "!", "-L", expected},
    {"test", "-d", expected},
    {"test", "!", "-L", filepath.Join(expected, ".git")},
    {"test", "-d", filepath.Join(expected, ".git")},
    {"realpath", "-e", "--", expected},
    managedGitCommand("git", "-C", expected, "rev-parse", "--show-toplevel"),
}
```

Compare trimmed `realpath` and Git top-level output to the exact expected path. On failure, return `errors.Join(contract.NewError(ErrorWorkspaceRepositoryUnavailable, "selected workspace repository is unavailable"), underlying)`.

- [ ] **Step 4: Write failing root/child environment and command-order tests**

Assert both provisioners:

- validate `WorkspaceStart` before connecting/allocating;
- set `environment.KANEDIAS_PI_WORKDIR` to `Workspace.Directory()`;
- call checkout validation after instance start and before `WaitRPC`;
- preserve cleanup ordering when validation fails;
- pass exactly the inherited workspace start for fresh and fork children.

Add `Exec` to child fakes only in tests first so compilation identifies every production interface seam.

- [ ] **Step 5: Run root/child provisioner tests and verify they fail**

Run: `go test ./internal/supervisor/provision -run 'Test(Root|Child)Provisioner.*(Workspace|Repository|Workdir)' -count=1`

Expected: FAIL because child clients/config and root config do not yet use the workspace start.

- [ ] **Step 6: Integrate validation into root and child provisioning**

Add `Exec` to `childIncusClient`. Set `environment.KANEDIAS_PI_WORKDIR` in root instance config and `applyChildConfig`. Call `prepareSessionWorkspace` after each successful `StartInstance` and before `waitForRootRPCAddress`/`WaitRPC`. Keep root's defensive ownership repair and child cleanup/recovery invariants intact.

- [ ] **Step 7: Write failing image bridge and launcher tests**

Extend environment-bridge tests to assert `KANEDIAS_PI_WORKDIR` is allowlisted and newline-safe. Add launcher cases that replace the literal production workspace root in the test copy with a temp root, then assert:

- absent value leaves cwd at the workspace root;
- selected valid checkout runs Pi with that cwd;
- paths outside the root, nested checkout names, missing directories, and symlinks fail before invoking Pi;
- argv remains unchanged and no `eval` is introduced.

Have the fake `pi` print `pwd -P` followed by one argument per line.

- [ ] **Step 8: Run image tests and verify they fail**

Run: `go test ./internal/image -run 'Test(PiEnvironmentBridge|PiRPCLauncher).*Work' -count=1`

Expected: FAIL because the bridge drops the variable and the launcher never changes directory.

- [ ] **Step 9: Implement the launcher defense**

Add `KANEDIAS_PI_WORKDIR` to `kanedias-pi-env`'s allowlist. In `kanedias-pi-rpc`:

```bash
workspace_root=/workspace
workdir=${KANEDIAS_PI_WORKDIR:-$workspace_root}
case "$workdir" in
    "$workspace_root") ;;
    "$workspace_root"/repos/*)
        checkout=${workdir#"$workspace_root"/repos/}
        [[ -n $checkout && $checkout != */* ]] || { echo "invalid Pi working directory" >&2; exit 1; }
        ;;
    *) echo "invalid Pi working directory" >&2; exit 1 ;;
esac
[[ -d $workdir && ! -L $workdir ]] || { echo "Pi working directory is unavailable" >&2; exit 1; }
[[ $(realpath -e -- "$workdir") == "$workdir" ]] || { echo "Pi working directory is unsafe" >&2; exit 1; }
cd -- "$workdir"
exec pi "${args[@]}"
```

The launcher is defense-in-depth only; provisioning remains the source of the typed browser failure.

- [ ] **Step 10: Run provisioning, image, and supervisor tests**

Run: `go test ./internal/supervisor/provision ./internal/image ./internal/supervisor -count=1`

Expected: PASS.

- [ ] **Step 11: Commit repository startup**

```bash
git add internal/supervisor/contract internal/supervisor/provision internal/image/kanedias-pi-env internal/image/kanedias-pi-rpc internal/image/image_test.go
git commit -m "feat: start sessions in configured repositories"
```

---

### Task 6: Report Root Startup Failures Without Losing Process-Exit Races

**Files:**
- Create: `internal/supervisor/process/root_status.go`
- Create: `internal/supervisor/process/root_status_test.go`
- Modify: `cmd/session.go`
- Modify: `cmd/session_runtime.go`
- Modify: `cmd/session_test.go`
- Modify: `cmd/session_runtime_test.go`
- Modify: `internal/manager/manager.go`
- Modify: `internal/manager/spawn.go`
- Modify: `internal/manager/spawn_test.go`

**Interfaces:**
- Produces: `process.RootStatusFD = 4`.
- Produces: `process.RootStartupStatus{Status string, Code contract.ErrorCode}` strict bounded codec.
- Extends: inherited root command with hidden `--status-fd 4`.
- Extends: `SessionOptions.RootStatus io.WriteCloser`.
- Consumed by: `Manager.admitRoot` concurrently with socket admission and process completion.

- [ ] **Step 1: Write failing strict startup-status codec tests**

Cover ready, repository failure, internal failure, unknown status, missing/extra code, unknown JSON field, trailing value, oversize input, and short writes:

```go
func TestRootStartupStatusStrictBoundedRoundTrip(t *testing.T) {
    for _, status := range []RootStartupStatus{
        {Status: RootStartupReady},
        {Status: RootStartupFailure, Code: contract.ErrorWorkspaceRepositoryUnavailable},
    } { /* Encode then Decode and compare */ }
}
```

The wire record contains no free-form message.

- [ ] **Step 2: Run codec tests and verify they fail**

Run: `go test ./internal/supervisor/process -run 'TestRootStartupStatus' -count=1`

Expected: FAIL because the protocol does not exist.

- [ ] **Step 3: Implement the one-record strict bounded protocol**

Use the existing `strictDecode`/`MaxRecordBytes`; validate exactly one of:

```go
const (
    RootStartupReady = "ready"
    RootStartupFailure = "failure"
)

type RootStartupStatus struct {
    Status string             `json:"status"`
    Code   contract.ErrorCode `json:"code,omitempty"`
}
```

`Ready` forbids a code. `Failure` requires a known nonempty code. Encoding must reject short writes.

- [ ] **Step 4: Write failing root command/runtime descriptor tests**

Extend `cmd/session_test.go` to pass separate bootstrap/status pipes, assert exact FD validation (`status >= 4`, distinct from bootstrap), assert both descriptors close on decode/config/runtime errors, and assert hidden defaults. In runtime tests, assert `node.Start` success writes ready and closes; a joined `ErrorWorkspaceRepositoryUnavailable` writes failure with that code; a plain error writes `ErrorInternal`.

- [ ] **Step 5: Run cmd tests and verify they fail**

Run: `go test ./cmd -run 'TestSession.*(Status|Startup)' -count=1`

Expected: FAIL because the command/runtime have no status writer.

- [ ] **Step 6: Thread and close the child-side status writer**

Open fd 4 with `syscall.CloseOnExec`, pass an `*os.File` through `SessionOptions.RootStatus`, and close it exactly once after writing the startup outcome immediately after `node.Start` returns. Direct CLI roots leave it nil. Reporting a ready-status failure is fatal and triggers node cleanup rather than leaving an unadmittable orphan.

- [ ] **Step 7: Write failing manager descriptor and race tests**

Add deterministic cases to `spawn_test.go`:

1. status failure arrives before process exit;
2. process `Done()` closes before the status-reader goroutine is scheduled, but the pipe contains repository failure;
3. status says ready and process exits before admission → generic failure;
4. malformed/oversize status → generic failure with log detail only;
5. successful admission closes manager status read and root inherited write endpoints;
6. start/bootstrap/status-pipe failures close every descriptor and preserve existing SIGKILL bootstrap-abort behavior.

Assert `ExtraFiles` are exactly fd 3 bootstrap read and fd 4 status write, and argv ends with both hidden flags.

- [ ] **Step 8: Run manager spawn tests and verify they fail**

Run: `go test ./internal/manager -run 'TestSpawnRoot.*(Status|Repository|Descriptor|Race)' -count=1`

Expected: FAIL because only one inherited pipe exists and `admitRoot` selects process exit generically.

- [ ] **Step 9: Implement manager-side concurrent status admission**

Add a `newRootStatusPipe` seam to `Manager`, create the second pipe before spawn, and start one bounded decoder goroutine after the process starts. Add status state to `pendingRoot` with an idempotent close.

In `admitRoot`:

- return a fixed manager-created `contract.Error` when a failure status arrives;
- treat ready as supplementary and continue socket admission;
- if `process.Done()` wins, synchronously receive the status result because process exit guarantees the inherited writer is closed, then prefer a valid failure code over generic exit;
- never trust or display a root-supplied message (none exists);
- close the manager read endpoint after success or failure;
- schedule failed-spawn cleanup exactly once in `SpawnRootWithRequest`.

- [ ] **Step 10: Run process/cmd/manager tests including race**

Run: `go test -race ./internal/supervisor/process ./cmd ./internal/manager -count=1`

Expected: PASS.

- [ ] **Step 11: Commit structured root status**

```bash
git add internal/supervisor/process/root_status.go internal/supervisor/process/root_status_test.go cmd/session.go cmd/session_runtime.go cmd/session_test.go cmd/session_runtime_test.go internal/manager/manager.go internal/manager/spawn.go internal/manager/spawn_test.go
git commit -m "feat: report typed root startup failures"
```

---

### Task 7: Store, Project, and Rename In-Memory Root Names

**Files:**
- Modify: `internal/manager/discovery.go`
- Modify: `internal/manager/spawn.go`
- Modify: `internal/manager/manager.go`
- Modify: `internal/manager/types.go`
- Create: `internal/manager/names_test.go`
- Test: `internal/manager/discovery_test.go`
- Test: `internal/manager/spawn_test.go`

**Interfaces:**
- Produces: `Manager.RenameRoot(sessionID, name string) error`.
- Extends: `RootState.Name string` and `SessionState.RootName string` as optional custom names.
- Preserves: root ID remains every route key and action identity.

- [ ] **Step 1: Write failing admission/projection/rename tests**

Cover:

- launch name commits only after admission;
- failed launch leaves no handle/name;
- fleet projects custom name;
- child session projects its root's custom name;
- empty name projects empty custom name and view fallback remains the ID;
- duplicate names across roots succeed;
- rename trims and changes both fleet/session revisions;
- clearing succeeds;
- same-value rename is a no-op;
- descendant/missing target returns typed not-found/invalid error;
- discovery-created handles have empty names;
- concurrent same-socket handle reuse receives the launch name.

- [ ] **Step 2: Run name tests and verify they fail**

Run: `go test ./internal/manager -run 'Test(RootName|RenameRoot|SpawnRoot.*Name)' -count=1`

Expected: FAIL because manager handles and public state have no names.

- [ ] **Step 3: Store launch names safely at admission**

Add `name string` to `rootHandle`, guarded by `Manager.mu`. Carry the already normalized `pendingRoot.name` into `commitSpawn`. After `commitTree` returns, set `res.handle.name` under `m.mu` so concurrent discovery reuse cannot discard the launch metadata. Do not put the name in a supervisor snapshot.

- [ ] **Step 4: Project root names and implement rename**

Populate:

```go
type RootState struct { Name string /* existing fields */ }
type SessionState struct { RootName string /* existing fields */ }
```

Implement `RenameRoot` by reusing the exact Task 3 normalizer, resolving `m.routes[sessionID]`, requiring `sessionID == rootID`, locating the live root handle, and changing only `handle.name`. Unlock before calling `bumpFleetRevision()` and `bumpSessionRevision()`. Skip bumps for no-op values.

- [ ] **Step 5: Run manager tests including race**

Run: `go test -race ./internal/manager -count=1`

Expected: PASS.

- [ ] **Step 6: Commit manager naming**

```bash
git add internal/manager/discovery.go internal/manager/spawn.go internal/manager/manager.go internal/manager/types.go internal/manager/names_test.go internal/manager/discovery_test.go internal/manager/spawn_test.go
git commit -m "feat: add in-memory root display names"
```

---

### Task 8: Add Name and Repository Controls to New Session

**Files:**
- Modify: `internal/server/view.go`
- Modify: `internal/server/actions.go`
- Modify: `internal/server/actions_test.go`
- Modify: `internal/server/handler_test.go`
- Modify: `internal/server/web/session-modal.html`
- Modify: `internal/server/web/session-modal.js`
- Modify: `internal/server/web/session-modal.test.js`
- Modify: `internal/server/web/app.css`

**Interfaces:**
- Consumes: `SessionLaunchOptions.Repositories`, `SessionLaunchRequest.Name`, and `.Repository`.
- Produces: modal fields `[data-session-name]` and `[data-start-repository]`.
- Maps: `ErrorWorkspaceRepositoryUnavailable` to fixed modal copy and HTTP 503.

- [ ] **Step 1: Write failing Go template/action tests**

Extend the initial-page test to require one optional text input, a `/workspace` default option, each configured slug exactly once in sorted order, and no absolute checkout path/credential. Extend new-session action tests to assert exact forwarding of `Name`/`Repository` and add this error mapping:

```go
{name: "repository unavailable",
 err: contract.NewError(contract.ErrorWorkspaceRepositoryUnavailable, "/private/path"),
 wantStatus: http.StatusServiceUnavailable,
 wantBody: `{"error":"The selected repository is not present in the workspace."}`},
```

Assert the response omits `/private/path` while logs retain it.

- [ ] **Step 2: Run server tests and verify they fail**

Run: `go test ./internal/server -run 'Test(InitialPageRendersSessionModal|NewSessionAction)' -count=1`

Expected: FAIL because view/modal/error mapping do not include the new fields.

- [ ] **Step 3: Project repository options and map the typed error**

Add a repository option view containing only `Slug` and a selected-default marker. `newIndexView` copies/sorts options and always renders the empty `/workspace` option first. In `makeNewSessionHandler`, branch on `ErrorWorkspaceRepositoryUnavailable`, keep status 503, and emit only the fixed copy.

- [ ] **Step 4: Write failing browser request/reset tests**

Extend the fake fixture with a text input and repository select. Require:

```js
assert.deepEqual(modalUI.buildRequest(f.dialog), {
  name: "release triage",
  repository: "owner/repo",
  root: { /* existing */ },
  workers: [ /* existing */ ]
});
```

Assert opening/reset clears the name, restores the empty repo option, pending disables both fields, failed launch preserves user-entered values, and successful launch resets them.

- [ ] **Step 5: Run modal tests and verify they fail**

Run: `node --test internal/server/web/session-modal.test.js`

Expected: FAIL because the fixture/controller do not query or submit these fields.

- [ ] **Step 6: Implement modal fields and request wiring**

Add accessible labels and controls:

```html
<label for="session-name">Session name <span class="optional">optional</span></label>
<input id="session-name" type="text" maxlength="80" autocomplete="off" data-session-name>
<label for="start-repository">Starting repository</label>
<select id="start-repository" data-start-repository>
  <option value="" selected>/workspace</option>
  {{range .Repositories}}<option value="{{.Slug}}">{{.Slug}}</option>{{end}}
</select>
```

Have `buildRequest` read raw values; server validation remains authoritative. Include inputs in pending/reset behavior and style inputs consistently with modal selects.

- [ ] **Step 7: Run browser and server tests**

Run: `node --test internal/server/web/session-modal.test.js && go test ./internal/server -count=1`

Expected: PASS.

- [ ] **Step 8: Commit launch UI**

```bash
git add internal/server/view.go internal/server/actions.go internal/server/actions_test.go internal/server/handler_test.go internal/server/web/session-modal.html internal/server/web/session-modal.js internal/server/web/session-modal.test.js internal/server/web/app.css
git commit -m "feat: configure new session names and repositories"
```

---

### Task 9: Render and Rename Root Names in the GUI

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/handler.go`
- Modify: `internal/server/actions.go`
- Modify: `internal/server/signals.go`
- Modify: `internal/server/view.go`
- Modify: `internal/server/actions_test.go`
- Modify: `internal/server/handler_test.go`
- Modify: `internal/server/manager_integration_test.go`
- Modify: `internal/server/web/fleet.html`
- Modify: `internal/server/web/detail.html`
- Modify: `internal/server/web/app.css`

**Interfaces:**
- Extends: server `fleetManager` with `RenameRoot(sessionID, name string) error`.
- Produces: authenticated same-origin `POST /ui/sessions/{sessionID}/name` accepting strict Datastar `{name}` signals.
- Consumes: `RootState.Name` and `SessionState.RootName`.

- [ ] **Step 1: Write failing manager/view tests for display fallback**

Add view tests asserting:

- root row name is custom when present, otherwise `RootSessionID`;
- root detail heading/breadcrumb use the custom name;
- child breadcrumb uses `RootName` then child worker/session label;
- immutable IDs remain in `data-session-id`, action URLs, and Session metric;
- hostile HTML in a name is escaped.

- [ ] **Step 2: Write failing rename action/security tests**

Add tests for valid, clear, malformed signal, descendant rejection from fake manager, manager failure sanitization/logging, unauthenticated request, cross-origin request, and disabled/no-fleet route. Verify the valid handler invokes `RenameRoot("root-1", "new name")` and returns a deck-status SSE patch.

- [ ] **Step 3: Run server tests and verify they fail**

Run: `go test ./internal/server -run 'Test.*(RootName|Rename)' -count=1`

Expected: FAIL because the interface, route, action, and views do not exist.

- [ ] **Step 4: Wire the root-only rename endpoint**

Add:

```go
type renameSignals struct { Name string `json:"name"` }
```

Register the route inside the existing authenticated write group and a matching NotFound route when writes are disabled. The handler strictly decodes signals, calls `fleet.RenameRoot`, and uses `patchDeckStatusAction` so failures remain sanitized while logs retain causes.

- [ ] **Step 5: Render names without changing identity attributes**

Add helper fields to `rootView`/`detailView`, computing fallback in Go:

```go
func displayRootName(name, rootID string) string {
    if name != "" { return name }
    return rootID
}
```

Use the display value only in text nodes. Keep every `data-session-id`, selected signal, request path, and Session metric on immutable IDs.

- [ ] **Step 6: Add the Datastar rename editor**

On root detail only, render a native `<details class="session-name-editor">` with Edit name summary, bounded input prefilled from the optional custom name, Save, and Cancel. Use the established safe Datastar element-query pattern:

```html
data-on:submit="@post('/ui/sessions/'+el.dataset.sessionId+'/name', {payload:{name:el.querySelector('input').value}})"
```

Cancel removes the `open` attribute without a request. A failed response leaves the details open and patches deck status; success revisions re-render fleet/detail/breadcrumb.

- [ ] **Step 7: Add an integration test for revision-driven rename updates**

In `manager_integration_test.go`, subscribe/render fleet and selected child detail, call the rename route, and assert both streams eventually contain escaped new root text while immutable IDs remain unchanged.

- [ ] **Step 8: Run server and browser regression tests**

Run: `go test -race ./internal/server -count=1 && node --test internal/server/web/*.test.js`

Expected: PASS.

- [ ] **Step 9: Commit rename GUI**

```bash
git add internal/server/server.go internal/server/handler.go internal/server/actions.go internal/server/signals.go internal/server/view.go internal/server/actions_test.go internal/server/handler_test.go internal/server/manager_integration_test.go internal/server/web/fleet.html internal/server/web/detail.html internal/server/web/app.css
git commit -m "feat: rename active root sessions"
```

---

### Task 10: Full Verification and Release Readiness

**Files:**
- Modify only if verification exposes a defect in the approved scope.
- Review: `docs/superpowers/specs/2026-08-10-session-names-and-repository-start-design.md`
- Review: `docs/superpowers/plans/2026-08-10-session-names-and-repository-start.md`

**Interfaces:**
- Verifies every prior task as one integrated launch/name/repository/workspace flow.

- [ ] **Step 1: Run formatting and generated-source checks**

```bash
gofmt -w $(git diff --name-only --diff-filter=ACM -- '*.go')
git diff --check
```

Expected: no formatting or whitespace errors.

- [ ] **Step 2: Run all unit and browser tests**

```bash
go test ./...
node --test internal/server/web/*.test.js
```

Expected: PASS.

- [ ] **Step 3: Run focused race suites**

```bash
go test -race ./internal/config ./internal/manager ./internal/server ./internal/supervisor/... ./internal/workspace
```

Expected: PASS with no race reports.

- [ ] **Step 4: Run static analysis**

```bash
go vet ./...
```

Expected: PASS.

- [ ] **Step 5: Inspect the effective artifacts rather than only exit codes**

Review the final diff and rendered template contracts. Confirm:

- names appear only in text/value presentation positions;
- IDs remain in routes/data attributes;
- modal repository values are slugs only;
- root/child configs set the same workdir;
- root status carries no free-form message;
- all pipe endpoints close on success/failure;
- provisioner validation precedes Pi dial/readiness;
- ownership repair runs for empty and nonempty workspace config;
- no live Incus resource was created during unit verification.

- [ ] **Step 6: Run independent correctness, security, test, and simplicity review**

Request fresh-context reviewers against the actual branch diff. Disposition every finding as blocker, fix-now, optional/deferred, or invalid. Apply accepted fixes with one writer and rerun affected commands.

- [ ] **Step 7: Commit any review fixes**

```bash
git add -u
git commit -m "fix: address session launch review"
```

Skip this commit if review requires no changes.

- [ ] **Step 8: Re-run final verification after review fixes**

```bash
go test ./...
go test -race ./internal/config ./internal/manager ./internal/server ./internal/supervisor/... ./internal/workspace
go vet ./...
node --test internal/server/web/*.test.js
git diff --check
git status --short
```

Expected: all pass and no uncommitted source changes.

- [ ] **Step 9: Prepare PR evidence**

Record the worktree path, branch, commit list, changed-file summary, exact test commands, root-status race coverage, the read-only live ownership observation (`/workspace root:root 0711`, `/workspace/repos kanedias:kanedias 0755`), and the explicit fact that destructive live acceptance was not run.
