# Kanedias CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a small Cobra CLI that renders embedded profiles and runs the existing credential proxy on a config-backed Incus bridge.

**Architecture:** `internal/config` validates TOML, `internal/network` ensures the fixed `kanedias` bridge, `internal/profiles` renders embedded templates, and `internal/proxy` exposes the existing proxy as callable operations. The `cmd` package coordinates those units through `proxy run`, `proxy init-ca`, `proxy login openai-codex`, and `profile <type>`.

**Tech Stack:** Go 1.26.5, Cobra, pelletier/go-toml/v2, `embed`, `text/template`, `net/netip`, Incus CLI, existing goproxy and Prometheus libraries.

## Global Constraints

- Use a root persistent `--config` flag with default `./config.toml`.
- The fixed managed Incus bridge name is `kanedias`.
- IPv4 is required; IPv6 is optional.
- The public proxy listen address is always `<configured IPv4 address>:3128`; do not expose `--listen`.
- Keep an internal proxy listen option for tests.
- Only `proxy run` ensures the Incus network. CA initialization and login must not touch networking.
- Keep Cobra's generated commands enabled.
- Do not modify existing shell scripts or shell test harnesses.
- Use test-driven development: demonstrate each new test failing before implementation, then passing.
- Run implementation agents in managed Git worktrees. Merge their commits into `main` only after their lane passes its tests.
- Config is the dependency base. After it lands, profiles, network, and proxy lanes may run in parallel. Build `cmd` only after those lanes are merged.
- Per user direction, do not run intermediate review agents. Perform one independent review after all implementation commits have been merged.

---

## File Structure

### New files

- `internal/config/config.go` — TOML structs, loading, and IPv4/IPv6 prefix validation.
- `internal/config/config_test.go` — config loading and validation tests.
- `internal/profiles/profiles.go` — embedded profile lookup and template rendering.
- `internal/profiles/profiles_test.go` — type selection and generated proxy URL tests.
- `internal/network/network.go` — Incus bridge lookup, validation, and creation.
- `internal/network/network_test.go` — fake-runner tests for all network states.
- `internal/proxy/service.go` — default proxy options and callable run/init/login operations.
- `internal/proxy/service_test.go` — public operation and default-path tests.
- `cmd/root.go` — root command, persistent config flag, dependency wiring, and execution.
- `cmd/profile.go` — `profile <type>` command.
- `cmd/proxy.go` — nested proxy command hierarchy and flags.
- `cmd/root_test.go` — command hierarchy and orchestration tests.
- `main.go` — executable entrypoint.

### Moved files

- `profiles/image-build.yaml` → `internal/profiles/image-build.yaml`.
- `profiles/lemonade.yaml` → `internal/profiles/lemonade.yaml`.
- `profiles/sandbox.yaml` → `internal/profiles/sandbox.yaml` and convert its proxy URLs to template expressions.
- Every current `proxy/*.go` and `proxy/*_test.go` file → `internal/proxy/`, with package declaration changed from `main` to `proxy`.

### Modified files

- `go.mod` and `go.sum` — add TOML and Cobra dependencies.

Existing shell files remain byte-for-byte unchanged even though their old direct `profiles/` and `proxy/` paths will be stale until the follow-up migration.

---

### Task 1: Config Package

**Execution:** One implementation agent in a managed worktree. Merge this commit into `main` before launching Tasks 2–4.

**Files:**
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Produces: `type Config struct { Network Network }`
- Produces: `type Network struct { IPv4 string; IPv6 string }`
- Produces: `func Load(path string) (Config, error)`
- Produces: `func (Network) IPv4Prefix() (netip.Prefix, error)`
- Produces: `func (Network) IPv6Prefix() (netip.Prefix, bool, error)`
- Consumers: Tasks 2, 3, 4, and 6 import `github.com/sklarsa/kanedias/internal/config`.

- [ ] **Step 1: Add failing config tests**

Create table-driven tests covering a valid IPv4-only file, valid dual-stack file, and failures. The assertions must include these cases:

```go
func TestLoad(t *testing.T) {
    tests := []struct {
        name    string
        content string
        want4   string
        want6   string
        wantErr string
    }{
        {name: "ipv4 only", content: "[network]\nipv4 = \"10.76.111.1/24\"\n", want4: "10.76.111.1/24"},
        {name: "dual stack", content: "[network]\nipv4 = \"10.76.111.1/24\"\nipv6 = \"fd42:28e2:2375:7000::1/64\"\n", want4: "10.76.111.1/24", want6: "fd42:28e2:2375:7000::1/64"},
        {name: "missing ipv4", content: "[network]\n", wantErr: "network.ipv4 is required"},
        {name: "invalid ipv4", content: "[network]\nipv4 = \"bad\"\n", wantErr: "network.ipv4"},
        {name: "wrong ipv4 family", content: "[network]\nipv4 = \"fd42::1/64\"\n", wantErr: "must be IPv4"},
        {name: "invalid ipv6", content: "[network]\nipv4 = \"10.76.111.1/24\"\nipv6 = \"bad\"\n", wantErr: "network.ipv6"},
        {name: "wrong ipv6 family", content: "[network]\nipv4 = \"10.76.111.1/24\"\nipv6 = \"10.0.0.1/24\"\n", wantErr: "must be IPv6"},
    }
    // Write each case to t.TempDir(), call Load, and inspect parsed prefix values.
}
```

Also test an unreadable path and malformed TOML separately so their errors identify reading and decoding respectively. For IPv6 omission, assert `IPv6Prefix()` returns `present == false` and no error.

- [ ] **Step 2: Run the config tests and verify red**

Run:

```bash
go test ./internal/config
```

Expected: FAIL because `internal/config` does not exist.

- [ ] **Step 3: Add the TOML dependency**

Run:

```bash
go get github.com/pelletier/go-toml/v2@latest
```

Expected: `go.mod` and `go.sum` include pelletier/go-toml/v2.

- [ ] **Step 4: Implement config loading and validation**

Use this structure and behavior:

```go
package config

import (
    "fmt"
    "net/netip"
    "os"

    "github.com/pelletier/go-toml/v2"
)

type Config struct {
    Network Network `toml:"network"`
}

type Network struct {
    IPv4 string `toml:"ipv4"`
    IPv6 string `toml:"ipv6"`
}

func Load(path string) (Config, error) {
    data, err := os.ReadFile(path)
    if err != nil {
        return Config{}, fmt.Errorf("read config %q: %w", path, err)
    }
    var cfg Config
    if err := toml.Unmarshal(data, &cfg); err != nil {
        return Config{}, fmt.Errorf("decode config %q: %w", path, err)
    }
    if _, err := cfg.Network.IPv4Prefix(); err != nil {
        return Config{}, err
    }
    if _, _, err := cfg.Network.IPv6Prefix(); err != nil {
        return Config{}, err
    }
    return cfg, nil
}
```

`IPv4Prefix` must reject an empty value, parsing errors, and non-IPv4 addresses. `IPv6Prefix` must return `(netip.Prefix{}, false, nil)` for an empty value and reject parsing errors or non-IPv6 addresses otherwise. Wrap errors with the exact setting name (`network.ipv4` or `network.ipv6`). Preserve host bits in the parsed prefix because the address is the bridge gateway and proxy listener.

- [ ] **Step 5: Run and format the config package**

Run:

```bash
gofmt -w internal/config/*.go
go test ./internal/config
go test ./...
```

Expected: all commands PASS; existing packages remain green.

- [ ] **Step 6: Commit the dependency base**

```bash
git add internal/config go.mod go.sum
git commit -m "feat: load kanedias config"
```

Expected: one self-contained config commit and a clean agent worktree.

---

### Task 2: Embedded Profiles Package

**Execution:** Run in parallel with Tasks 3 and 4, each in its own managed worktree based on `main` after Task 1 is merged.

**Files:**
- Move: `profiles/image-build.yaml` → `internal/profiles/image-build.yaml`
- Move: `profiles/lemonade.yaml` → `internal/profiles/lemonade.yaml`
- Move and modify: `profiles/sandbox.yaml` → `internal/profiles/sandbox.yaml`
- Create: `internal/profiles/profiles.go`
- Create: `internal/profiles/profiles_test.go`

**Interfaces:**
- Consumes: `config.Config` and `config.Network.IPv4Prefix()` from Task 1.
- Produces: constants `Sandbox`, `ImageBuild`, and `Lemonade` of type `Type`.
- Produces: `func Types() []string` returning `image-build`, `lemonade`, and `sandbox` in lexical order.
- Produces: `func Render(w io.Writer, name string, cfg config.Config) error`.
- Consumer: Task 6 wires `Render` to the Cobra profile command.

- [ ] **Step 1: Move the YAML inputs without changing content yet**

```bash
mkdir -p internal/profiles
git mv profiles/image-build.yaml internal/profiles/image-build.yaml
git mv profiles/lemonade.yaml internal/profiles/lemonade.yaml
git mv profiles/sandbox.yaml internal/profiles/sandbox.yaml
rmdir profiles
```

Expected: Git records three renames; no shell files are edited.

- [ ] **Step 2: Write failing rendering tests**

Tests must assert:

```go
func TestRenderSandboxUsesConfiguredIPv4(t *testing.T) {
    cfg := config.Config{Network: config.Network{IPv4: "10.76.111.1/24"}}
    var output bytes.Buffer
    if err := Render(&output, "sandbox", cfg); err != nil {
        t.Fatal(err)
    }
    for _, key := range []string{
        "environment.HTTP_PROXY", "environment.HTTPS_PROXY",
        "environment.http_proxy", "environment.https_proxy",
    } {
        want := key + ": \"http://10.76.111.1:3128\""
        if !strings.Contains(output.String(), want) {
            t.Errorf("rendered sandbox missing %q", want)
        }
    }
    if strings.Contains(output.String(), "10.75.177.1") {
        t.Fatal("rendered sandbox retained the old hard-coded endpoint")
    }
}
```

Add a table test that renders `image-build`, `lemonade`, and `sandbox`, checks non-empty output ending in a newline, and confirms each expected description. Add an unknown-type test that requires the error to contain the bad name and all supported names. Add an invalid sandbox IPv4 test requiring a `network.ipv4` error. Assert `Types()` returns a fresh lexical-order slice so callers cannot mutate package state.

- [ ] **Step 3: Run profile tests and verify red**

```bash
go test ./internal/profiles
```

Expected: FAIL because `Render`, `Types`, and the package source do not exist.

- [ ] **Step 4: Convert sandbox proxy values into template fields**

Change only the four HTTP/HTTPS proxy values in `internal/profiles/sandbox.yaml`:

```yaml
  environment.HTTP_PROXY: "{{ .ProxyURL }}"
  environment.HTTPS_PROXY: "{{ .ProxyURL }}"
  environment.http_proxy: "{{ .ProxyURL }}"
  environment.https_proxy: "{{ .ProxyURL }}"
```

Keep all other profile content unchanged.

- [ ] **Step 5: Implement embedded template rendering**

Use `//go:embed *.yaml`, a stable type map, `text/template`, and a private render-data struct:

```go
type Type string

const (
    ImageBuild Type = "image-build"
    Lemonade   Type = "lemonade"
    Sandbox    Type = "sandbox"
)

type templateData struct {
    ProxyURL string
}

func Types() []string {
    return []string{string(ImageBuild), string(Lemonade), string(Sandbox)}
}
```

`Render` must reject names not in the type map before reading a file. For `sandbox`, parse the IPv4 prefix and set `ProxyURL` with:

```go
prefix, err := cfg.Network.IPv4Prefix()
proxyURL := "http://" + net.JoinHostPort(prefix.Addr().String(), "3128")
```

Although the current listener is IPv4, use `net.JoinHostPort` so formatting remains correct. Parse each embedded file with `template.New(name).Option("missingkey=error")`, then execute directly into the caller's writer. Wrap read, parse, and execute errors with the profile name.

- [ ] **Step 6: Format and verify the profiles lane**

```bash
gofmt -w internal/profiles/*.go
go test ./internal/profiles
go test ./...
git diff --check
```

Expected: all commands PASS; no shell files appear in `git diff --name-only`.

- [ ] **Step 7: Commit the profiles package**

```bash
git add -A -- internal/profiles profiles
git commit -m "feat: embed Incus profiles"
```

Expected: one profiles commit and a clean agent worktree.

---

### Task 3: Incus Network Package

**Execution:** Run in parallel with Tasks 2 and 4 in an isolated worktree based on Task 1.

**Files:**
- Create: `internal/network/network.go`
- Create: `internal/network/network_test.go`

**Interfaces:**
- Consumes: `config.Config`, `IPv4Prefix()`, and `IPv6Prefix()` from Task 1.
- Produces: `const Name = "kanedias"`.
- Produces: `func Ensure(ctx context.Context, cfg config.Config) error`.
- Keeps private: `runner` interface and `ensure(ctx, runner, cfg)` test seam.
- Consumer: Task 6 calls `network.Ensure` only from `proxy run`.

- [ ] **Step 1: Write a fake Incus runner and failing tests**

The private seam must be:

```go
type runner interface {
    Run(ctx context.Context, args ...string) ([]byte, error)
}
```

Build a scripted fake that records each argument slice and returns queued output/error pairs. Cover these exact scenarios:

1. Empty JSON list creates the bridge with:
   ```text
   network create kanedias --type=bridge ipv4.address=10.76.111.1/24
   ```
2. IPv6 config appends:
   ```text
   ipv6.address=fd42:28e2:2375:7000::1/64
   ```
3. A matching JSON record returns successfully without a create call.
4. An existing `managed:false` record fails with `not managed`.
5. An existing `type:"physical"` record fails with `must be a bridge`.
6. A differing IPv4 address fails and includes both actual and expected values.
7. A configured, differing IPv6 address fails and includes both values.
8. Omitted config IPv6 ignores an arbitrary existing `ipv6.address`.
9. Malformed list JSON fails with `decode Incus network list`.
10. A list command error is propagated and does not trigger creation.
11. A create command error is propagated.
12. Invalid direct `config.Config` values fail before any runner call.

Use list JSON shaped like:

```json
[{"name":"kanedias","type":"bridge","managed":true,"config":{"ipv4.address":"10.76.111.1/24","ipv6.address":"fd42:28e2:2375:7000::1/64"}}]
```

Every test must first expect this lookup call:

```text
network list name=kanedias --format=json
```

- [ ] **Step 2: Run network tests and verify red**

```bash
go test ./internal/network
```

Expected: FAIL because the network package does not exist.

- [ ] **Step 3: Implement Incus execution and bridge reconciliation**

Define only the JSON fields consumed:

```go
type incusNetwork struct {
    Name    string            `json:"name"`
    Type    string            `json:"type"`
    Managed bool              `json:"managed"`
    Config  map[string]string `json:"config"`
}
```

The production runner uses:

```go
exec.CommandContext(ctx, "incus", args...).CombinedOutput()
```

It wraps failures with the command arguments and trimmed command output without losing the original error.

`ensure` must validate config prefixes first, execute the exact filtered JSON list call, and require either zero or one exact-name result. Zero results trigger creation. More than one exact-name result is an error. Validate managed status and type before address values. Parse actual address strings with `netip.ParsePrefix` and compare parsed prefixes so IPv6 textual compression differences are accepted. Do not mutate an existing network.

- [ ] **Step 4: Format and verify the network lane**

```bash
gofmt -w internal/network/*.go
go test ./internal/network
go test ./...
git diff --check
```

Expected: all commands PASS.

- [ ] **Step 5: Commit the network package**

```bash
git add internal/network
git commit -m "feat: ensure Incus proxy network"
```

Expected: one network commit and a clean agent worktree.

---

### Task 4: Importable Proxy Package

**Execution:** Run in parallel with Tasks 2 and 3 in an isolated worktree based on Task 1.

**Files:**
- Move: all `proxy/*.go` → `internal/proxy/`
- Create: `internal/proxy/service.go`
- Create: `internal/proxy/service_test.go`
- Modify after move: `internal/proxy/proxy.go` (formerly `proxy/main.go`)
- Modify after move: every Go file package declaration

**Interfaces:**
- Produces:
  ```go
  type Options struct {
      ListenAddress         string
      MetricsListenAddress  string
      RequestLog            bool
      CACertPath            string
      CAKeyPath             string
      ClaudeCredentialsPath string
      OpenAICodexAuthPath   string
      Logger                *slog.Logger
  }
  ```
- Produces: `func DefaultOptions() (Options, error)`.
- Produces: `func Run(options Options) error`.
- Produces: `func InitCA(certPath, keyPath string) error`.
- Produces: `func LoginOpenAICodex(ctx context.Context, authPath string, out io.Writer) error`.
- Consumer: Task 6 binds Cobra flags and supplies configured `ListenAddress`.

- [ ] **Step 1: Move the proxy package and establish a compiling package name**

```bash
mkdir -p internal/proxy
git mv proxy/*.go internal/proxy/
rmdir proxy
for file in internal/proxy/*.go; do
    perl -0pi -e 's/^package main$/package proxy/m' "$file"
done
git mv internal/proxy/main.go internal/proxy/proxy.go
```

At this point the moved package still compiles, but it retains the obsolete `func main`, flag parsing, and process-exit behavior that Step 4 will remove.

- [ ] **Step 2: Add failing service tests**

Add tests that:

- set `HOME` and `XDG_CONFIG_HOME`, call `DefaultOptions`, and require these defaults:
  - `127.0.0.1:3128` listen;
  - `<XDG_CONFIG_HOME>/kanedias-proxy/ca.crt` and `ca.key`;
  - `<HOME>/.claude/.credentials.json`;
  - `<XDG_CONFIG_HOME>/kanedias-proxy/openai-codex.json`;
- call `InitCA` with temporary paths, assert both files exist, certificate mode is `0644`, key mode is `0600`, and a second call succeeds;
- call `LoginOpenAICodex` with a canceled context and temporary auth path, asserting a non-nil error without invoking a real browser login.

Keep all existing proxy tests in the same `proxy` package so they continue testing private helpers.

- [ ] **Step 3: Run the moved package tests and verify red**

```bash
go test ./internal/proxy
```

Expected: FAIL because the service API is not implemented and the old `main` still owns execution.

- [ ] **Step 4: Extract service operations from the old main function**

Remove `flag` and `os.Exit` handling from `proxy.go`. Keep credential, proxy construction, authority, token, CA, and certificate helpers there.

Implement `DefaultOptions` by calling `os.UserConfigDir()` and `os.UserHomeDir()`, preserving the existing `defaultOAuthPaths` helper and `kanedias-proxy` directory layout.

Implement:

```go
func InitCA(certPath, keyPath string) error {
    _, _, err := loadOrCreateCA(certPath, keyPath)
    return err
}

func LoginOpenAICodex(ctx context.Context, authPath string, out io.Writer) error {
    return newOpenAICodexOAuthSource(authPath).Login(ctx, out)
}
```

`Run` must contain the old server-start path: initialize/load the CA, create optional metrics, create the observer and proxy handler, launch the proxy and optional metrics HTTP servers, log listener details, block for a server failure, and return the wrapped error instead of exiting. If `options.Logger` is nil, use a text `slog` logger writing to stderr. Preserve existing server timeout values and log fields.

Reject an empty internal listen address with a clear error. Do not add signal management or unrelated lifecycle behavior.

- [ ] **Step 5: Update stale Go-only invocation text**

In the build-tagged live proxy test, replace the old remediation text `go run ./proxy -login-openai-codex` with `go run . proxy login openai-codex`. Do not edit shell files.

- [ ] **Step 6: Format and verify the proxy lane**

```bash
gofmt -w internal/proxy/*.go
go test ./internal/proxy
go test ./...
git diff --check
```

Expected: all non-live tests PASS and no `proxy/` directory remains.

- [ ] **Step 7: Commit the importable proxy package**

```bash
git add -A -- internal/proxy proxy
git commit -m "refactor: expose proxy as internal package"
```

Expected: one proxy commit and a clean agent worktree.

---

### Task 5: Merge Parallel Package Lanes

**Execution:** Parent orchestrator on `main`, after Tasks 2–4 finish. This is an integration checkpoint, not a code-review checkpoint.

**Files:**
- Merge the committed changes from Tasks 2, 3, and 4.

**Interfaces:**
- Confirms all package interfaces named above coexist before Cobra work starts.

- [ ] **Step 1: Merge each successful worktree commit into `main`**

Use the exact commit IDs returned in the managed-worktree handoffs. Cherry-pick the profiles handoff first, the network handoff second, and the proxy handoff third, one commit at a time without squashing. Record each resulting `HEAD` before proceeding.

Expected: no path conflicts because each lane owns a distinct directory.

- [ ] **Step 2: Run package integration tests without review**

```bash
go test ./...
git status --short
git diff --check
```

Expected: Go tests PASS. The only pre-existing untracked file may be the user's root `config.toml`; do not add, overwrite, or delete it during package merges.

---

### Task 6: Cobra CLI and Entrypoint

**Execution:** One implementation agent in a new managed worktree based on `main` after Task 5.

**Files:**
- Create: `cmd/root.go`
- Create: `cmd/profile.go`
- Create: `cmd/proxy.go`
- Create: `cmd/root_test.go`
- Create: `main.go`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:**
- Consumes: `config.Load`, `network.Ensure`, `profiles.Render`, and all Task 4 proxy operations.
- Produces: `func Execute() error`.
- Keeps private: `services`, `newRootCommand(services, proxy.Options)`, and focused subcommand constructors for dependency-injected tests.

- [ ] **Step 1: Add Cobra**

```bash
go get github.com/spf13/cobra@latest
```

Expected: Cobra and its required indirect modules appear in `go.mod` and `go.sum`.

- [ ] **Step 2: Write failing command hierarchy tests**

Define a private dependency bundle in production code with these fields so tests can substitute fakes:

```go
type services struct {
    loadConfig       func(string) (config.Config, error)
    ensureNetwork    func(context.Context, config.Config) error
    renderProfile    func(io.Writer, string, config.Config) error
    runProxy         func(proxy.Options) error
    initCA           func(string, string) error
    loginOpenAICodex func(context.Context, string, io.Writer) error
}
```

Tests must build the root with deterministic proxy defaults and assert:

- `profile`, `proxy`, `proxy run`, `proxy init-ca`, `proxy login`, and `proxy login openai-codex` resolve;
- `proxy run` has no `listen` flag;
- `proxy run` retains `metrics-listen`, `request-log`, `ca-cert`, `ca-key`, `claude-credentials`, and `openai-codex-auth`;
- `proxy init-ca` has `ca-cert` and `ca-key`;
- `proxy login openai-codex` has `openai-codex-auth`;
- root `config` default is `./config.toml`;
- an unsupported or missing profile argument returns an error;
- generated Cobra behavior is not disabled.

Run:

```bash
go test ./cmd
```

Expected: FAIL because command constructors do not exist.

- [ ] **Step 3: Write failing orchestration tests**

Use recorded calls and command `SetArgs`, `SetOut`, and `SetErr` to verify:

```go
root.SetArgs([]string{"--config", "/tmp/custom.toml", "profile", "sandbox"})
```

calls `loadConfig("/tmp/custom.toml")`, passes `sandbox` and that config to the renderer, and emits the renderer's bytes on command stdout.

For:

```go
root.SetArgs([]string{"proxy", "run", "--request-log", "--metrics-listen", "127.0.0.1:9090"})
```

require call order `load`, `ensure`, `run`; require `run` receives `ListenAddress == "10.76.111.1:3128"`, request logging enabled, metrics address set, and all default path values retained.

Run both `proxy init-ca` and `proxy login openai-codex` in separate tests and assert neither `loadConfig` nor `ensureNetwork` is called. Verify each operation receives explicit path flags and login receives the Cobra command's output writer.

Run:

```bash
go test ./cmd
```

Expected: FAIL because orchestration is not implemented.

- [ ] **Step 4: Implement the root and profile commands**

`root.go` must:

- define real services from the imported internal packages;
- obtain `proxy.DefaultOptions()` in `Execute`;
- construct `Use: "kanedias"` with a short description;
- set `SilenceUsage: true` so runtime errors stay concise;
- bind persistent `--config` to a closure-owned string defaulting to `./config.toml`;
- add profile and proxy commands;
- leave Cobra completion generation at its default.

`profile.go` must use `cobra.ExactArgs(1)`, set `ValidArgs` from `profiles.Types()`, load config in `RunE`, and render directly to `cmd.OutOrStdout()`.

- [ ] **Step 5: Implement the nested proxy commands**

The hierarchy is exact:

```text
proxy
├── run
├── init-ca
└── login
    └── openai-codex
```

`proxy run` loads the root config, obtains the required IPv4 prefix, sets:

```go
options.ListenAddress = net.JoinHostPort(prefix.Addr().String(), "3128")
```

then calls `ensureNetwork(cmd.Context(), cfg)` before `runProxy(options)`. Copy default options per command construction so flag mutations do not leak across tests.

The run flags map directly to all `Options` fields except `ListenAddress` and `Logger`. The init and login commands invoke only their respective service operation and do not load config.

- [ ] **Step 6: Add the root executable**

Create:

```go
package main

import (
    "os"

    "github.com/sklarsa/kanedias/cmd"
)

func main() {
    if err := cmd.Execute(); err != nil {
        os.Exit(1)
    }
}
```

Cobra owns normal error text; internal packages continue returning errors.

- [ ] **Step 7: Format and run CLI tests**

```bash
gofmt -w cmd/*.go main.go
go test ./cmd
go test ./...
git diff --check
```

Expected: all commands PASS.

- [ ] **Step 8: Run command smoke tests in the worktree**

Create a temporary config rather than depending on the user's untracked root file:

```bash
config_file=$(mktemp)
cat >"$config_file" <<'EOF'
[network]
ipv4 = "10.76.111.1/24"
ipv6 = "fd42:28e2:2375:7000::1/64"
EOF

go run . --config "$config_file" profile sandbox | grep -F 'environment.HTTPS_PROXY: "http://10.76.111.1:3128"'
go run . --config "$config_file" profile image-build | grep -F 'description: Unprivileged container'
go run . --config "$config_file" profile lemonade | grep -F 'description: Expose the host Lemonade GPU service'
go run . proxy --help
go run . proxy login --help
rm -f "$config_file"
```

Expected: all commands exit zero. Do not run `proxy run`, because it would mutate Incus and start a long-running listener.

- [ ] **Step 9: Commit the CLI**

```bash
git add cmd main.go go.mod go.sum
git commit -m "feat: add kanedias Cobra CLI"
```

Expected: one CLI commit and a clean agent worktree.

---

### Task 7: Merge CLI and Verify the Complete Implementation

**Execution:** Parent orchestrator on `main`; no review agent yet.

**Files:**
- Merge the Task 6 commit.

- [ ] **Step 1: Merge the CLI worktree commit**

Cherry-pick the exact commit ID returned by the CLI managed-worktree handoff.

Expected: the CLI commit lands after all internal package commits without conflict.

- [ ] **Step 2: Run fresh full verification on `main`**

```bash
go test ./...
go vet ./...
git diff --check
git status --short
```

Expected: tests and vet PASS, diff check is empty, and `config.toml` remains the only allowed pre-existing untracked file.

- [ ] **Step 3: Run profile and hierarchy smoke tests on `main`**

```bash
go run . --config ./config.toml profile sandbox | tee /tmp/kanedias-sandbox.yaml
grep -F 'environment.HTTP_PROXY: "http://10.76.111.1:3128"' /tmp/kanedias-sandbox.yaml
grep -F 'environment.https_proxy: "http://10.76.111.1:3128"' /tmp/kanedias-sandbox.yaml
go run . --config ./config.toml profile image-build >/tmp/kanedias-image-build.yaml
go run . --config ./config.toml profile lemonade >/tmp/kanedias-lemonade.yaml
go run . proxy --help
go run . proxy login --help
rm -f /tmp/kanedias-sandbox.yaml /tmp/kanedias-image-build.yaml /tmp/kanedias-lemonade.yaml
```

Expected: all commands exit zero and both proxy casing variants point to the configured IPv4 address.

---

### Task 8: One Final Independent Review

**Execution:** Only now dispatch one fresh read-only reviewer, preferably in a managed worktree. This is the sole review stage requested by the user.

**Files:**
- Review all implementation commits after `af66eb2` against `docs/superpowers/specs/2026-08-06-kanedias-cli-design.md`.

- [ ] **Step 1: Request final review with evidence**

Give the reviewer the spec, plan, commit range, and fresh verification output. Require findings ordered by severity and focused on correctness, security, command behavior, regression risk, and missing tests. Explicitly require the reviewer to check:

- only `proxy run` can invoke Incus;
- an existing mismatched network is never modified;
- optional IPv6 semantics match the spec;
- the proxy has no public listen override;
- template output contains no unexpanded delimiters;
- internal proxy code no longer exits the process;
- existing Go proxy behavior remains covered;
- shell scripts were not edited.

- [ ] **Step 2: Address review findings in one writer worktree if needed**

If the reviewer reports actionable issues, dispatch one implementation agent in a managed worktree based on current `main`. The agent must reproduce each issue with a failing test, implement the smallest correction, run targeted tests and `go test ./...`, and commit fixes. Merge that single fix commit into `main`.

If no actionable issues are reported, make no review-only changes.

- [ ] **Step 3: Run final post-review verification**

```bash
go test ./...
go vet ./...
git diff --check
git status --short
git log --oneline --decorate -8
```

Expected: all verification commands PASS, implementation commits are on `main`, no implementation worktree remains unmerged, and the user's `config.toml` remains untracked and unchanged.
