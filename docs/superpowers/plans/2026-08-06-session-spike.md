# Kanedias Session Spike Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `kanedias session`, which reads one prompt from stdin, runs one Pi RPC process in an ephemeral Incus sandbox, streams raw RPC JSONL over TCP, and deletes the session resources.

**Architecture:** Every Incus client is scoped to an automatically ensured hardcoded `kanedias` project. Kanedias images, profiles, instances, and custom volumes live in that project, while its Incus-managed bridge remains in the default network project and is shared through `features.networks=false`. The bridge supplies direct IPv4 NAT egress for image construction; workspace synchronization and sandbox/session traffic continue to use the externally managed Kanedias proxy. The guest base image uses systemd socket activation to connect TCP port 7777 directly to `pi --mode rpc` stdin/stdout, while a focused Go session workflow owns Incus lifecycle, address discovery, raw JSONL forwarding, and cleanup.

**Tech Stack:** Go 1.26.5, Cobra, github.com/lxc/incus/v7 v7.3.0, systemd socket activation, Pi RPC JSONL, TCP.

## Global Constraints

- Run every writer in a separate managed Git worktree; do not mutate the user's current working tree during implementation.
- Execute Tasks 1, 2, and 3 in parallel because their files are disjoint.
- Merge those commits into one integration worktree before Tasks 4 and 5.
- Run exactly one independent code review after all implementation tasks are integrated and verified. Do not dispatch task-level review agents.
- Kanedias images, profiles, instances, and custom volumes use the hardcoded project name `kanedias`.
- The Incus-managed bridge remains in the default network project and is shared through `features.networks=false`, because bridge networks cannot be project-local. Network operations still flow through the project-scoped client, and Incus maps them to the default network project.
- The managed bridge requires `ipv4.nat=true` for direct image-build egress. Image creation does not use the Kanedias proxy; `kanedias proxy run` remains a prerequisite for workspace synchronization and sandbox/session traffic.
- A missing project is created with isolated images, profiles, and storage volumes plus access to the shared default-project bridge; an existing project with incompatible feature values fails clearly.
- `kanedias session` accepts no arguments and reads the complete prompt from stdin.
- Stdout contains only raw Pi RPC JSONL. Progress and Kanedias errors go to stderr.
- The existing `kanedias proxy run` process is a prerequisite and is not started by the session command.
- Pi RPC transport is ordinary TCP to guest port 7777. The session path must not use Incus exec.
- The guest uses systemd socket activation and standard shell only; add no bridge binary, `socat`, or `netcat` dependency.
- One invocation sends one prompt and ends at `agent_settled`; no reconnect, replay, multiple prompts, or authentication.
- Always remove the invocation-owned instance and cloned workspace on success, failure, or cancellation.
- Keep the spike direct. Do not introduce a generic orchestration framework or exhaustive RPC event handling.

---

## File Structure

### Incus project ownership

- Modify `internal/incusclient/client.go`: hardcoded project constant, project ensure/validation, project-scoped connection.
- Modify `internal/incusclient/client_test.go`: missing-project creation, incompatible feature rejection, project selection.

### Guest Pi endpoint

- Create `internal/image/kanedias-pi.socket`: TCP socket unit.
- Create `internal/image/kanedias-pi@.service`: inetd-style Pi service template.
- Create `internal/image/kanedias-pi-rpc`: NVM-aware Pi launcher.
- Modify `internal/image/image.go`: embed and upload the three guest files.
- Modify `internal/image/install.sh`: install the files and enable the socket.
- Modify `internal/image/image_test.go`: verify uploaded service inputs and modes/content.

### Host RPC and lifecycle

- Create `internal/session/rpc.go`: send one prompt, forward JSONL byte-for-byte, stop at `agent_settled`.
- Create `internal/session/rpc_test.go`: local stream tests.
- Create `internal/session/session.go`: Incus session lifecycle, address discovery, TCP connection, cleanup.
- Create `internal/session/session_test.go`: focused happy path and one cleanup failure test.
- Modify `internal/incusclient/instance.go`: context-aware instance state lookup.

### CLI

- Create `cmd/session.go`: stdin-only Cobra command.
- Modify `cmd/root.go`: service injection and command registration.
- Modify `cmd/root_test.go`: hierarchy, stdin delegation, empty input, and argument rejection.

---

## Parallel Wave 1

### Task 1: Hardcoded Incus Project

**Execution:** Dedicated managed worktree. Run in parallel with Tasks 2 and 3. Commit once; do not request a review.

**Files:**
- Modify: `internal/incusclient/client.go`
- Modify: `internal/incusclient/client_test.go`

**Interfaces:**
- Produces: `const ProjectName = "kanedias"`
- Produces: existing `func Connect(context.Context) (*Client, error)` now ensures and selects `ProjectName`
- Produces private seams: `func ensureProject(projectManager) error` and `func scopeProject(contextServer) (contextServer, error)`

- [ ] **Step 1: Add missing-project and validation tests**

Add a small fake that implements only the private project seam:

```go
type fakeProjectManager struct {
    project *api.Project
    getErr  error
    created *api.ProjectsPost
}

func (f *fakeProjectManager) GetProject(string) (*api.Project, string, error) {
    return f.project, "", f.getErr
}

func (f *fakeProjectManager) CreateProject(project api.ProjectsPost) error {
    f.created = &project
    return nil
}
```

Add `TestEnsureProjectCreatesMissingKanediasProject`. Return `api.StatusErrorf(http.StatusNotFound, "missing")`, call `ensureProject`, and assert:

```go
if fake.created.Name != ProjectName {
    t.Fatalf("created project = %q, want %q", fake.created.Name, ProjectName)
}
for key, want := range map[string]string{
    "features.images":          "true",
    "features.profiles":        "true",
    "features.networks":        "false",
    "features.storage.volumes": "true",
} {
    if fake.created.Config[key] != want {
        t.Errorf("created feature %q = %q, want %s", key, fake.created.Config[key], want)
    }
}
```

Add `TestEnsureProjectAcceptsRequiredFeatures` with an existing project containing images, profiles, and storage volumes set to `"true"` and networks set to `"false"`.

Add a table-driven `TestEnsureProjectRejectsIncompatibleFeatures` that sets each required isolated feature to `"false"` in turn and sets networks to `"true"`, requiring the error to contain both `ProjectName` and the key.

Add a project-selection test using an embedded upstream interface so the fake does not implement every Incus method:

```go
type fakeContextServer struct {
    incus.InstanceServer
    selected string
    scoped   incus.InstanceServer
}

func (f *fakeContextServer) UseProject(name string) incus.InstanceServer {
    f.selected = name
    return f.scoped
}

func (f *fakeContextServer) WithContext(context.Context) incus.InstanceServer {
    return f
}
```

Exercise `scopeProject(server)` and assert `selected == ProjectName` and that the returned value is the fake's scoped `contextServer`.

- [ ] **Step 2: Run the focused tests and verify red**

```bash
go test ./internal/incusclient -run 'TestEnsureProject|Test.*ProjectSelection' -count=1
```

Expected: compilation fails because the project constant and helpers do not exist.

- [ ] **Step 3: Implement project ensure and selection**

In `client.go`, add:

```go
const ProjectName = "kanedias"

var requiredProjectFeatures = map[string]string{
    "features.images":          "true",
    "features.profiles":        "true",
    "features.networks":        "false",
    "features.storage.volumes": "true",
}

type projectManager interface {
    GetProject(string) (*api.Project, string, error)
    CreateProject(api.ProjectsPost) error
}
```

Implement `ensureProject` directly:

- when `GetProject(ProjectName)` returns 404, call `CreateProject` with name `kanedias`, description `Kanedias managed resources`, and a copy of `requiredProjectFeatures`;
- propagate non-404 lookup and create errors with operation context;
- for an existing project, require every feature value to equal its configured required value and report the mismatched key.

Implement `scopeProject(server contextServer) (contextServer, error)` by calling `server.UseProject(ProjectName)`, asserting the result supports `contextServer`, and returning it. After the Unix connection and current `contextServer` assertion, call `ensureProject(contextual.WithContext(ctx))`, then `scopeProject(contextual)`, and store the result in `Client.server`. Disconnect the original server before returning on any setup error.

Do not update an incompatible existing project and do not migrate default-project resources.

- [ ] **Step 4: Run project and package tests**

```bash
gofmt -w internal/incusclient/client.go internal/incusclient/client_test.go
go test ./internal/incusclient -count=1
go test ./internal/network ./internal/profiles -count=1
git diff --check
```

Expected: all commands pass.

- [ ] **Step 5: Commit**

```bash
git add internal/incusclient/client.go internal/incusclient/client_test.go
git commit -m "feat: scope resources to Kanedias project"
```

Return the commit hash and test commands to the parent; do not launch a reviewer.

---

### Task 2: Systemd-Activated Pi RPC Endpoint

**Execution:** Dedicated managed worktree. Run in parallel with Tasks 1 and 3. Commit once; do not request a review.

**Files:**
- Create: `internal/image/kanedias-pi.socket`
- Create: `internal/image/kanedias-pi@.service`
- Create: `internal/image/kanedias-pi-rpc`
- Modify: `internal/image/image.go`
- Modify: `internal/image/install.sh`
- Modify: `internal/image/image_test.go`

**Interfaces:**
- Produces guest TCP endpoint `0.0.0.0:7777`
- Produces executable `/usr/local/libexec/kanedias-pi-rpc`
- Produces enabled `kanedias-pi.socket`

- [ ] **Step 1: Add failing upload assertions**

Extend `TestCreateRunsImageWorkflowInOrder` so the expected upload sequence includes:

```text
push /root/assets/kanedias-pi.socket
push /root/assets/kanedias-pi@.service
push /root/assets/kanedias-pi-rpc
```

Change the recording client's files map to retain both fields:

```go
type uploadedFile struct {
    content []byte
    mode    int
}
```

Update `PushFile` to store `uploadedFile{content: append([]byte(nil), content...), mode: mode}` and adjust existing content assertions to read `.content`. After the workflow, assert uploaded contents contain these exact protocol-critical values:

```go
socket := string(client.files["/root/assets/kanedias-pi.socket"].content)
if !strings.Contains(socket, "ListenStream=0.0.0.0:7777") ||
   !strings.Contains(socket, "Accept=yes") ||
   !strings.Contains(socket, "MaxConnections=1") {
    t.Fatalf("socket unit = %q", socket)
}
service := string(client.files["/root/assets/kanedias-pi@.service"].content)
for _, want := range []string{
    "User=kanedias",
    "WorkingDirectory=/workspace",
    "StandardInput=socket",
    "StandardOutput=inherit",
    "StandardError=journal",
} {
    if !strings.Contains(service, want) {
        t.Errorf("service unit missing %q", want)
    }
}
launcher := client.files["/root/assets/kanedias-pi-rpc"]
if !strings.Contains(string(launcher.content), "exec pi --mode rpc --no-session") {
    t.Fatalf("launcher = %q", launcher.content)
}
if launcher.mode != 0o700 {
    t.Errorf("launcher mode = %#o, want 0700", launcher.mode)
}
for _, path := range []string{
    "/root/assets/kanedias-pi.socket",
    "/root/assets/kanedias-pi@.service",
} {
    if client.files[path].mode != 0o644 {
        t.Errorf("%s mode = %#o, want 0644", path, client.files[path].mode)
    }
}
```

- [ ] **Step 2: Run the focused image test and verify red**

```bash
go test ./internal/image -run TestCreateRunsImageWorkflowInOrder -count=1
```

Expected: failure because the three inputs are not uploaded.

- [ ] **Step 3: Create the exact guest files**

Create `internal/image/kanedias-pi.socket`:

```ini
[Unit]
Description=Kanedias Pi RPC socket

[Socket]
ListenStream=0.0.0.0:7777
Accept=yes
MaxConnections=1
NoDelay=yes

[Install]
WantedBy=sockets.target
```

Create `internal/image/kanedias-pi@.service`:

```ini
[Unit]
Description=Kanedias Pi RPC session
After=network-online.target

[Service]
User=kanedias
Group=kanedias
Environment=HOME=/home/kanedias
WorkingDirectory=/workspace
ExecStart=/usr/local/libexec/kanedias-pi-rpc
StandardInput=socket
StandardOutput=inherit
StandardError=journal
```

Create executable source `internal/image/kanedias-pi-rpc`:

```bash
#!/usr/bin/env bash
set -Eeuo pipefail

export NVM_DIR=/home/kanedias/.nvm
# shellcheck source=/dev/null
source "$NVM_DIR/nvm.sh"
exec pi --mode rpc --no-session
```

- [ ] **Step 4: Embed and upload the guest files**

In `image.go`, add one `go:embed` variable per new file:

```go
//go:embed kanedias-pi.socket
var piRPCSocket []byte

//go:embed kanedias-pi@.service
var piRPCService []byte

//go:embed kanedias-pi-rpc
var piRPCLauncher []byte
```

Append all three to the existing `/root/assets` upload list with modes `0644`, `0644`, and `0700`, respectively.

In `install.sh`, add the three names to the initial required-file check. After `install_pi`, add `install_pi_rpc_service`:

```bash
install_pi_rpc_service() {
    install -d -m 0755 /usr/local/libexec
    install -m 0755 "$assets_dir/kanedias-pi-rpc" \
        /usr/local/libexec/kanedias-pi-rpc
    install -m 0644 "$assets_dir/kanedias-pi.socket" \
        /etc/systemd/system/kanedias-pi.socket
    install -m 0644 "$assets_dir/kanedias-pi@.service" \
        /etc/systemd/system/kanedias-pi@.service
    systemctl enable kanedias-pi.socket
}
```

Call `install_pi_rpc_service` immediately after `install_pi` at the bottom of the installer. Do not start the socket in the temporary image-build container.

- [ ] **Step 5: Verify units, shell, and image tests**

```bash
gofmt -w internal/image/image.go internal/image/image_test.go
shellcheck internal/image/install.sh internal/image/kanedias-pi-rpc
systemd-analyze verify internal/image/kanedias-pi.socket internal/image/kanedias-pi@.service
go test ./internal/image -count=1
git diff --check
```

Expected: all commands pass. If `systemd-analyze` reports only that `/workspace` or the launcher does not exist on the host, retain the unit files and record that host-environment-only warning; syntax errors must be fixed.

- [ ] **Step 6: Commit**

```bash
git add internal/image
git commit -m "feat: expose Pi RPC through systemd socket"
```

Return the commit hash and test commands to the parent; do not launch a reviewer.

---

### Task 3: Minimal Pi RPC Stream Client

**Execution:** Dedicated managed worktree. Run in parallel with Tasks 1 and 2. Commit once; do not request a review.

**Files:**
- Create: `internal/session/rpc.go`
- Create: `internal/session/rpc_test.go`

**Interfaces:**
- Produces private `func runRPC(context.Context, net.Conn, string, io.Writer) error`
- Sends request ID `prompt-1`
- Returns success only after forwarding `agent_settled`

- [ ] **Step 1: Write the successful stream test**

Use `net.Pipe`. The server goroutine decodes the first JSON command and asserts:

```go
if command.Type != "prompt" || command.ID != "prompt-1" ||
   command.Message != "first line\nsecond line\n" {
    t.Errorf("command = %#v", command)
}
```

Then write these records with trailing LF:

```json
{"id":"prompt-1","type":"response","command":"prompt","success":true}
{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"hello"}}
{"type":"agent_settled"}
```

Call `runRPC` and require stdout to equal those three records byte-for-byte, including final newlines.

Add one compact rejection test: the server writes a failed prompt response, `runRPC` forwards it, and the returned error contains the remote error text.

Add one incomplete-stream test: close the server after a successful prompt response without `agent_settled`; require a non-nil error.

- [ ] **Step 2: Run the package test and verify red**

```bash
go test ./internal/session -count=1
```

Expected: compilation fails because `runRPC` does not exist.

- [ ] **Step 3: Implement strict LF JSONL forwarding**

Create these minimal wire types in `rpc.go`:

```go
const promptRequestID = "prompt-1"

type promptCommand struct {
    ID      string `json:"id"`
    Type    string `json:"type"`
    Message string `json:"message"`
}

type rpcEnvelope struct {
    Type    string `json:"type"`
    Command string `json:"command,omitempty"`
    Success *bool  `json:"success,omitempty"`
    Error   string `json:"error,omitempty"`
}
```

`runRPC` must:

1. start a small cancellation goroutine that closes the connection when `ctx.Done()` fires;
2. encode one `promptCommand{ID: promptRequestID, Type: "prompt", Message: prompt}` with `json.Encoder`, which supplies the required LF;
3. use `bufio.Reader.ReadBytes('\n')`, not `bufio.Scanner`, so large JSON records do not hit Scanner's token limit;
4. reject EOF with a partial record;
5. unmarshal each complete record before writing it;
6. write the original bytes unchanged to the supplied writer;
7. return the RPC error when a `response` for command `prompt` has `success:false`;
8. return nil after forwarding `{"type":"agent_settled"}`;
9. treat EOF before settlement as an error.

Do not interpret text, thinking, tool, usage, or extension UI records.

- [ ] **Step 4: Run and format**

```bash
gofmt -w internal/session/rpc.go internal/session/rpc_test.go
go test ./internal/session -count=1
git diff --check
```

Expected: all commands pass.

- [ ] **Step 5: Commit**

```bash
git add internal/session/rpc.go internal/session/rpc_test.go
git commit -m "feat: stream Pi RPC records"
```

Return the commit hash and test commands to the parent; do not launch a reviewer.

---

## Integration Wave

Before Task 4, create one managed integration worktree from the design commit and cherry-pick the successful commits from Tasks 1, 2, and 3 in that order. Run:

```bash
go test ./internal/incusclient ./internal/image ./internal/session -count=1
git diff --check
```

Do not merge or edit in the user's current working tree. Tasks 4 and 5 use the same integration worktree sequentially so there is only one writer.

### Task 4: Ephemeral Incus Session Workflow

**Execution:** Integration worktree after Wave 1 is combined. Commit once; do not request a review.

**Files:**
- Modify: `internal/incusclient/instance.go`
- Create: `internal/session/session.go`
- Create: `internal/session/session_test.go`

**Interfaces:**
- Consumes: `incusclient.Connect(context.Context) (*incusclient.Client, error)` scoped to `kanedias`
- Consumes: `runRPC(context.Context, net.Conn, string, io.Writer) error`
- Produces: `func Run(context.Context, config.Config, string, io.Writer, io.Writer) error`
- Produces: `func (*incusclient.Client) GetImageAlias(context.Context, string) (*api.ImageAliasesEntry, error)`
- Produces: `func (*incusclient.Client) GetInstanceState(context.Context, string) (*api.InstanceState, error)`

- [ ] **Step 1: Write the focused happy-path workflow test**

Create a narrow recording fake implementing only the session client methods:

```go
type sessionClient interface {
    Disconnect()
    ResolvePool(context.Context, string) (string, error)
    GetNetwork(context.Context, string) (*api.Network, error)
    CreateNetwork(context.Context, api.NetworksPost) error
    EnsureProfile(context.Context, string, []byte) error
    GetImageAlias(context.Context, string) (*api.ImageAliasesEntry, error)
    GetStorageVolume(context.Context, string, string) (*api.StorageVolume, error)
    CopyStorageVolume(context.Context, string, string, string) error
    DeleteStorageVolume(context.Context, string, string) error
    CreateInstance(context.Context, api.InstancesPost) error
    StartInstance(context.Context, string) error
    StopInstance(context.Context, string, bool) error
    DeleteInstance(context.Context, string) error
    GetInstanceState(context.Context, string) (*api.InstanceState, error)
}
```

Inject dependencies through a private `dependencies` struct containing:

```go
connect          func(context.Context) (sessionClient, error)
ensureNetwork    func(context.Context, sessionClient, config.Config) error
renderProfile    func(io.Writer, string, config.Config) error
defaultProxyOpts func() (proxy.Options, error)
initCA           func(string, string) error
checkProxy       func(context.Context, config.Config) error
dialRPC          func(context.Context, string) (net.Conn, error)
newName          func() (string, error)
readinessTimeout time.Duration
retryInterval    time.Duration
```

Use `newName` returning `session-test`, an immediate instance state containing global IPv4 `10.76.111.42` on `eth0`, and `net.Pipe` for `dialRPC`. Have the peer validate the prompt and send a successful response plus `agent_settled`.

Assert the ordered lifecycle contains:

```text
resolve-pool
init-ca
ensure-network
ensure-profile sandbox
check-proxy
get-image sandbox
get-volume kanedias-workspace-seed
copy-volume kanedias-workspace-seed kanedias-workspace-session-test
create-instance
start-instance
get-instance-state
dial 10.76.111.42:7777
stop-instance
delete-instance
delete-volume kanedias-workspace-session-test
```

Assert the `api.InstancesPost` request has:

```go
Name:     "session-test"
Profiles: []string{"default", "sandbox"}
Source:   api.InstanceSource{Type: "image", Alias: cfg.BaseImage.Name}
Config: map[string]string{
    "user.kanedias.kind":     "session",
    "user.kanedias.rpc.port": "7777",
}
```

Require the local `workspace` device to use the resolved pool, source `kanedias-workspace-session-test`, and path `/workspace`. Require stdout to equal only the two raw RPC lines and progress to appear only on stderr.

- [ ] **Step 2: Write one cleanup failure-path test**

Make `StartInstance` return a sentinel error after instance creation. Assert the session returns that error, skips RPC dialing, deletes the created instance, and deletes the cloned volume. Record cleanup contexts and require they are not cancelled and have a deadline no later than 30 seconds.

Do not add a permutation test for each Incus operation.

- [ ] **Step 3: Run the session tests and verify red**

```bash
go test ./internal/session -run 'TestRun' -count=1
```

Expected: compilation fails because `Run`, `dependencies`, and the state adapter do not exist.

- [ ] **Step 4: Add context-aware image and instance-state lookups**

In `internal/incusclient/instance.go`, add:

```go
func (c *Client) GetImageAlias(ctx context.Context, name string) (*api.ImageAliasesEntry, error) {
    alias, _, err := c.server.WithContext(ctx).GetImageAlias(name)
    if err != nil {
        return nil, fmt.Errorf("get Incus image alias %q: %w", name, err)
    }
    return alias, nil
}

func (c *Client) GetInstanceState(ctx context.Context, name string) (*api.InstanceState, error) {
    state, _, err := c.server.WithContext(ctx).GetInstanceState(name)
    if err != nil {
        return nil, fmt.Errorf("get state for Incus instance %q: %w", name, err)
    }
    return state, nil
}
```

No Incus exec method is used by the session workflow.

- [ ] **Step 5: Implement the direct session workflow**

In `session.go`, define:

```go
const (
    rpcPort             = "7777"
    sandboxProfile      = "sandbox"
    workspaceDevice     = "workspace"
    workspacePath       = "/workspace"
    workspaceNamePrefix = "kanedias-workspace-"
    cleanupTimeout      = 30 * time.Second
)

func Run(ctx context.Context, cfg config.Config, prompt string, stdout, stderr io.Writer) error
```

`Run` calls this exact private seam for tests:

```go
func run(
    ctx context.Context,
    cfg config.Config,
    prompt string,
    stdout io.Writer,
    stderr io.Writer,
    deps dependencies,
) (err error)
```

The public function delegates with `run(ctx, cfg, prompt, stdout, stderr, defaultDependencies())`. Keep sequencing direct:

1. call `cfg.ValidateLifecycle()` and reject `strings.TrimSpace(prompt) == ""`;
2. connect, resolve the pool, initialize the proxy CA, ensure the network, render and ensure the sandbox profile, and check the external proxy;
3. verify the configured base image alias with `GetImageAlias` and the seed volume with `GetStorageVolume`;
4. generate a random `session-` name using `crypto/rand` and lowercase hex in production;
5. copy the seed to `workspaceNamePrefix + name`;
6. create the instance with the profiles, metadata, and local workspace device from the test contract;
7. start the instance;
8. call a private readiness helper that polls `GetInstanceState`, selects the first `eth0` address with `Family == "inet"` and `Scope == "global"`, and calls `dialRPC(ctx, net.JoinHostPort(address, rpcPort))` until it succeeds or the readiness context expires;
9. call `runRPC` with the connected socket;
10. use a named return and deferred cleanup to force-stop, delete the instance, and delete the volume with `context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)`.

Track `volumeCreated`, `instanceCreated`, and `instanceRunning`. Never delete a resource whose create operation did not succeed. Attempt later cleanup steps even when an earlier cleanup step fails, and join cleanup errors with the workflow error.

Default dependencies must use `incusclient.Connect`, `network.EnsureWithClient`, `profiles.Render`, `proxy.DefaultOptions`, `proxy.InitCA`, and `net.Dialer.DialContext`.

Implement the proxy prerequisite check as one bounded TCP dial to the configured IPv4 address on port 3128. Close it immediately on success. This only checks that the external listener exists; it does not start or manage the proxy.

Progress lines use concrete messages such as `Creating session <name>...`, `Waiting for Pi RPC in <name>...`, `Stopping session <name>...`, and `Deleting session <name>...`; all must go to stderr. Do not write any Kanedias text to stdout.

- [ ] **Step 6: Run focused and neighboring tests**

```bash
gofmt -w internal/incusclient/instance.go internal/session/*.go
go test ./internal/session ./internal/incusclient ./internal/sandbox -count=1
rg -n 'Exec\(' internal/session && exit 1 || true
git diff --check
```

Expected: tests pass and the search finds no Incus exec usage in `internal/session`.

- [ ] **Step 7: Commit**

```bash
git add internal/incusclient/instance.go internal/session
git commit -m "feat: run ephemeral Pi sessions"
```

Do not launch a reviewer.

---

### Task 5: `kanedias session` Cobra Command

**Execution:** Same integration worktree, after Task 4. Commit once; do not request a review.

**Files:**
- Create: `cmd/session.go`
- Modify: `cmd/root.go`
- Modify: `cmd/root_test.go`

**Interfaces:**
- Consumes: `session.Run(context.Context, config.Config, string, io.Writer, io.Writer) error`
- Adds service seam: `runSession func(context.Context, config.Config, string, io.Writer, io.Writer) error`
- Produces command: `kanedias session`

- [ ] **Step 1: Add failing command hierarchy and delegation tests**

Update the root hierarchy expectation to:

```go
assertChildCommands(t, root, "image", "profile", "proxy", "sandbox", "session", "workspace")
```

Add `TestSessionReadsPromptFromStdinAndDelegates`. Configure the root with:

```go
root.SetIn(strings.NewReader("first line\nsecond line\n"))
root.SetOut(&stdout)
root.SetErr(&stderr)
root.SetArgs([]string{"--config", "/tmp/session.toml", "session"})
```

The fake `loadConfig` returns a sentinel config. The fake `runSession` asserts the exact command context, config, prompt string, stdout writer, and stderr writer, then writes one JSONL record to stdout. Require load then run call order.

Add `TestSessionRejectsEmptyInputBeforeWorkflow` for both `""` and whitespace-only stdin. Require a non-nil error and zero `runSession` calls.

Add `{"session", "extra"}` to argument-rejection coverage. Require the command to expose no local flags.

- [ ] **Step 2: Run command tests and verify red**

```bash
go test ./cmd -run 'TestCommandHierarchyAndFlags|TestSession' -count=1
```

Expected: failure because the command and service seam do not exist.

- [ ] **Step 3: Implement the command**

Add the service field and production wiring in `root.go`:

```go
runSession func(context.Context, config.Config, string, io.Writer, io.Writer) error
```

Set it to `session.Run` in `realServices`, register `newSessionCommand`, and initialize it in `stubServices`.

Create `cmd/session.go`:

```go
func newSessionCommand(service services, configPath func() string) *cobra.Command {
    return &cobra.Command{
        Use:   "session",
        Short: "Run one prompt in an ephemeral Pi sandbox",
        Args:  cobra.NoArgs,
        RunE: func(cmd *cobra.Command, _ []string) error {
            prompt, err := io.ReadAll(cmd.InOrStdin())
            if err != nil {
                return fmt.Errorf("read session prompt: %w", err)
            }
            if strings.TrimSpace(string(prompt)) == "" {
                return fmt.Errorf("session prompt on stdin is empty")
            }
            cfg, err := service.loadConfig(configPath())
            if err != nil {
                return err
            }
            return service.runSession(
                cmd.Context(), cfg, string(prompt),
                cmd.OutOrStdout(), cmd.ErrOrStderr(),
            )
        },
    }
}
```

Reading and validating stdin must occur before config loading or Incus work.

- [ ] **Step 4: Run command and full unit tests**

```bash
gofmt -w cmd/session.go cmd/root.go cmd/root_test.go
go test ./cmd ./internal/session -count=1
go test ./... -count=1
git diff --check
```

Expected: all commands pass.

- [ ] **Step 5: Commit**

```bash
git add cmd/session.go cmd/root.go cmd/root_test.go
git commit -m "feat: add session command"
```

Do not launch a reviewer.

---

### Task 6: Final Verification, Smoke Test, and One Review

**Execution:** Integration worktree after all implementation commits. This is the only review phase.

- [ ] **Step 1: Run fresh static and unit verification**

```bash
gofmt -w cmd internal
go test ./... -count=1
go vet ./...
shellcheck internal/image/install.sh internal/image/kanedias-pi-rpc
systemd-analyze verify internal/image/kanedias-pi.socket internal/image/kanedias-pi@.service
git diff --check
rg -n 'Exec\(' internal/session && exit 1 || true
git status --short
```

Expected: tests, vet, shellcheck, and diff check pass; session contains no Incus exec call; the tree is clean. Record any `systemd-analyze` warning caused solely by host paths absent outside the built image.

- [ ] **Step 2: Build the CLI**

```bash
go build -o /tmp/kanedias-session-spike .
/tmp/kanedias-session-spike session --help
```

Expected: build succeeds and help identifies stdin-driven ephemeral Pi session behavior.

- [ ] **Step 3: Prepare and run the live smoke test**

With a working local Incus daemon, use the user's existing ignored configuration and assets from the original checkout while keeping all code changes in the integration worktree. Image creation uses the bridge's direct IPv4 NAT egress and does not require `kanedias proxy run`:

```bash
KANEDIAS_CONFIG=/home/steven/source/github/kanedias/config.toml
/tmp/kanedias-session-spike --config "$KANEDIAS_CONFIG" image create
```

After image creation completes, start `proxy run` with the same config in a separately managed process if it is not already running. Keep it running for both workspace synchronization and the sandbox session:

```bash
/tmp/kanedias-session-spike --config "$KANEDIAS_CONFIG" proxy run &
PROXY_PID=$!
/tmp/kanedias-session-spike --config "$KANEDIAS_CONFIG" workspace sync
printf '%s\n' 'Reply with a short greeting.' | \
    /tmp/kanedias-session-spike --config "$KANEDIAS_CONFIG" session \
    > /tmp/kanedias-session.jsonl
kill "$PROXY_PID"
```

Validate the captured stream:

```bash
jq -c . < /tmp/kanedias-session.jsonl >/dev/null
grep -q '"type":"agent_settled"' /tmp/kanedias-session.jsonl
incus project show kanedias
```

Finally confirm no invocation-owned session instance or volume remains in the `kanedias` project. If an external prerequisite prevents this smoke test, preserve the unit-test results and report the exact command and environmental error rather than changing scope.

- [ ] **Step 4: Dispatch exactly one independent final reviewer**

Give a fresh read-only reviewer the design, plan, complete integrated diff, and verification output. Ask only for concrete correctness, safety, protocol, cleanup, and requirement findings. Do not ask for stylistic expansion and do not dispatch additional reviewers.

- [ ] **Step 5: Apply valid final findings in the integration worktree**

The integration writer evaluates each finding technically, applies only fixes required by the approved spike, and creates one fix commit when changes are needed:

```bash
git add -A
git commit -m "fix: address final session review"
```

If no finding requires a change, do not create an empty commit.

- [ ] **Step 6: Re-run final verification after review fixes**

```bash
go test ./... -count=1
go vet ./...
shellcheck internal/image/install.sh internal/image/kanedias-pi-rpc
git diff --check
git status --short
```

Expected: all commands pass and the integration worktree is clean.

- [ ] **Step 7: Report the integration handoff**

Return the integration branch/worktree handoff, ordered commits, verification evidence, live smoke result, final review findings and dispositions, and any residual environmental risk. Do not modify or merge into the user's current working tree without explicit instruction.
