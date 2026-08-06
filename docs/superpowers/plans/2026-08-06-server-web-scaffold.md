# Kanedias Server Web Scaffold Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver `kanedias server --listen 127.0.0.1:8080` as a local-only, gracefully shutting down web server with a fully embedded offline Datastar shell and no application state or Incus behavior.

**Architecture:** A sequential foundation commit establishes the stable `internal/server` API, loopback-only validation, HTTP lifecycle, logging, timeouts, and shutdown behavior. From that exact commit, disjoint web and Cobra lanes run in parallel; one integration writer then combines them, verifies the integrated binary and smoke behavior, dispositions exactly one fresh-context final review, and preserves the managed integration branch and native handoff artifact.

**Tech Stack:** Go 1.26.5, Cobra v1.10.2, Chi v5.2.3, `html/template`, `embed`, `log/slog`, `net/http`, official `github.com/starfederation/datastar-go` v1.2.2, and the official Datastar browser bundle v1.0.2.

## Global Constraints

- The approved design commits are already applied in this order and remain the authority for implementation:
  1. `8f68972d8510d38dc070ab65c47b48733f07d4e8`
  2. `47956b4a5478c0e32dcf9bb757d795094c75bf53`
- The approved design is `docs/superpowers/specs/2026-08-06-server-web-scaffold-design.md`. Stop and escalate rather than changing a product or architecture decision that conflicts with it.
- The parent must dispatch every writer with the harness-native managed `worktree:true` mechanism. No participant may run `git worktree add`, create a manual worktree, edit the user's checkout, or merge into the user's checkout.
- A lane may run `git merge --ff-only "$FOUNDATION_COMMIT"` in its own isolated managed branch before editing, followed by an exact `HEAD` check. It must not create another worktree.
- Before foundation edits, run `go mod download` and `go test ./... -count=1`. If either baseline command fails, stop without source edits and report the failure.
- Use red/green TDD for every behavior change. Each writer records the red and green commands, expected result, actual result, commit hash, and changed-file list in its native handoff artifact.
- The foundation lane is sequential. Only after its commit exists may the web and Cobra lanes run in parallel from that exact foundation commit.
- Parallel ownership is strict and disjoint:
  - Web lane: `internal/server/handler.go`, `internal/server/handler_test.go`, `internal/server/web/**`, `go.mod`, and `go.sum`.
  - Cobra lane: `cmd/server.go`, `cmd/root.go`, and `cmd/root_test.go`.
- Do not launch task-level or lane reviewers. After integrated verification and curl smoke pass, launch exactly one independent fresh-context final reviewer. The integration/fix writer dispositions findings; do not launch a second reviewer.
- Keep the server local and offline. Accept only case-insensitive exact `localhost`, IPv4 loopback literals, and IPv6 loopback literals. Reject empty, wildcard, unspecified, private-LAN, public, and arbitrary hostname binds before listening. Permit port `0`.
- Browser resources must all be flat files below `internal/server/web`: `index.html`, `app.css`, `datastar.js`, `datastar.LICENSE`, and `datastar.PROVENANCE`. Embed with `//go:embed web/*`. Do not create `internal/server/templates` or `internal/server/assets`.
- Pin `github.com/go-chi/chi/v5` to v5.2.3 and `github.com/starfederation/datastar-go` to v1.2.2. Vendor the official Datastar v1.0.2 browser bundle unchanged and record its verified SHA-256 and upstream license/provenance.
- Do not add runtime downloads, CDN references, npm, lockfiles, bundlers, Templ, generators, generated frontend source, or filesystem-served runtime assets.
- The page status refresh is click-only. It must not request status on load or create a persistent event loop.
- Do not add Incus calls, authentication, authorization, databases, persistence, sessions, state mutation, forms, state-changing routes, or configuration-file dependencies.
- Retain generic panic/error responses, injected structured logging, explicit content types, local-only validation, bounded graceful shutdown, and request logging from the approved design.
- Do not add CSP, `X-Content-Type-Options`, `Referrer-Policy`, or a general security-header middleware or tests. A strict CSP is outside the approved design and may interfere with Datastar expression evaluation.
- Configure `ReadHeaderTimeout: 5 * time.Second`, `IdleTimeout: 60 * time.Second`, and `WriteTimeout: 0`. Cancellation must call `Shutdown` with a fresh background-derived 10-second timeout.
- Keep `main.go` byte-for-byte unchanged. Install SIGINT/SIGTERM cancellation in `cmd.Execute`, and test exact context propagation through its `execute` helper into `services.runServer`.
- Preserve the managed integration branch and native handoff artifact after completion. Do not promise a temporary filesystem path because the harness may clean managed worktree paths automatically.

---

## Approved Interfaces and File Map

The foundation commit creates this stable package API; neither parallel lane may alter it:

```go
package server

import (
    "context"
    "log/slog"
)

const DefaultListenAddress = "127.0.0.1:8080"

type Options struct {
    ListenAddress string
    Logger        *slog.Logger
}

func ValidateListenAddress(address string) error
func Run(ctx context.Context, options Options) error
```

The foundation uses these private seams:

```go
type listenFunc func(network, address string) (net.Listener, error)

func prepareOptions(options Options) (Options, error)
func newHandler(logger *slog.Logger) (http.Handler, error)
func newHTTPServer(address string, handler http.Handler) *http.Server
func run(
    ctx context.Context,
    options Options,
    handler http.Handler,
    listen listenFunc,
    shutdownTimeout time.Duration,
) error
```

Cobra service injection adds exactly one field while leaving all existing fields unchanged:

```go
type services struct {
    // Existing fields remain unchanged.
    runServer func(context.Context, server.Options) error
}
```

Production wiring and command construction are:

```go
runServer: server.Run,

func newServerCommand(service services) *cobra.Command
```

Signal-aware production execution and its test seam are:

```go
func Execute() error {
    options, err := proxy.DefaultOptions()
    if err != nil {
        return err
    }

    ctx, stop := signal.NotifyContext(
        context.Background(),
        os.Interrupt,
        syscall.SIGTERM,
    )
    defer stop()

    return execute(ctx, realServices(), options)
}

func execute(ctx context.Context, service services, options proxy.Options) error {
    return newRootCommand(service, options).ExecuteContext(ctx)
}
```

File responsibilities:

- Create `internal/server/server.go`: stable API, listen validation, server construction, lifecycle logging, serve loop, and graceful shutdown.
- Create `internal/server/server_test.go`: validation, timeout, listener, serve, and cancellation tests.
- Create then replace `internal/server/handler.go`: inert foundation handler first; embedded Chi/Datastar router in the web lane.
- Create `internal/server/handler_test.go`: routes, methods, initial HTML, inert panels, embedding, SSE, panic recovery, request logging, and cancellation tests.
- Create `internal/server/web/index.html`: full HTML shell and initial process-status element.
- Create `internal/server/web/app.css`: local shell styling.
- Create `internal/server/web/datastar.js`: unchanged official Datastar v1.0.2 browser bundle.
- Create `internal/server/web/datastar.LICENSE`: corresponding upstream license.
- Create `internal/server/web/datastar.PROVENANCE`: immutable source, release, retrieval, path, and digest record.
- Create `cmd/server.go`: Cobra command, listen flag, stderr logger, validation, and service delegation.
- Modify `cmd/root.go`: server injection, command registration, signal context, and `execute` helper.
- Modify `cmd/root_test.go`: hierarchy, command contract, injection, stderr logger, and exact canceled-context propagation.
- Modify `go.mod` and `go.sum`: exact Chi v5.2.3 and datastar-go v1.2.2 pins.
- Do not modify `main.go` or any existing Incus, image, proxy, sandbox, workspace, network, profile, or configuration package.

### Task 1: Establish the Managed Foundation Lane and Baseline

**Files:** No source changes.

**Interfaces:** Use the approved interfaces above without modification.

- [ ] **Step 1: Dispatch the foundation writer natively**

The parent dispatches one foundation writer with `worktree:true` at the plan commit. The writer records:

```bash
PLAN_COMMIT=$(git rev-parse HEAD)
git branch --show-current
git rev-parse --show-toplevel
git status --short --untracked-files=all
```

Expected: a harness-managed isolated branch/worktree at the plan commit and empty status. No manual worktree command appears in the handoff.

- [ ] **Step 2: Download existing modules and run the clean baseline**

Run:

```bash
go mod download
go test ./... -count=1
```

Expected: both commands exit `0`. If either fails, stop without editing and return the exact command and failure.

### Task 2: Implement the Stable Server Foundation with TDD

**Files:**
- Create: `internal/server/server.go`
- Create: `internal/server/server_test.go`
- Create: `internal/server/handler.go`

**Interfaces:** Implement the stable package API and private seams under “Approved Interfaces and File Map” exactly.

- [ ] **Step 1: Write failing listen-validation tests**

Create table-driven tests with these exact cases:

```go
func TestValidateListenAddressAcceptsLocalOnlyAddresses(t *testing.T) {
    cases := []string{
        "127.0.0.1:8080",
        "127.0.0.1:0",
        "LOCALHOST:8080",
        "[::1]:8080",
        "[0:0:0:0:0:0:0:1]:0",
    }
    // Each case must return nil.
}

func TestValidateListenAddressRejectsNonLocalAddresses(t *testing.T) {
    cases := []string{
        "",
        ":8080",
        "0.0.0.0:8080",
        "[::]:8080",
        "192.168.1.2:8080",
        "8.8.8.8:8080",
        "example.com:8080",
        "127.0.0.1",
        "127.0.0.1:http",
        "127.0.0.1:-1",
        "127.0.0.1:65536",
    }
    // Each case must return a non-nil error containing the input address.
}
```

Use subtests and report the complete address in failures.

- [ ] **Step 2: Run the validation tests RED**

Run:

```bash
go test ./internal/server -run '^TestValidateListenAddress' -count=1
```

Expected: FAIL to compile because `ValidateListenAddress` does not exist.

- [ ] **Step 3: Implement minimal loopback-only validation**

Implement with `net.SplitHostPort`, `strconv.ParseUint(port, 10, 16)`, `strings.EqualFold(host, "localhost")`, `net.ParseIP`, and `IP.IsLoopback`. Reject an empty host, malformed/missing port, wildcard or unspecified IP, non-loopback IP, and every hostname other than exact case-insensitive `localhost`. Do not resolve arbitrary hostnames. Wrap errors with the rejected address and operation context.

- [ ] **Step 4: Run validation tests GREEN**

Run:

```bash
gofmt -w internal/server/server.go internal/server/server_test.go
go test ./internal/server -run '^TestValidateListenAddress' -count=1
```

Expected: PASS for every accepted and rejected table entry.

- [ ] **Step 5: Write failing timeout and lifecycle tests**

Add:

```text
TestRunRejectsNilContext
TestRunRejectsNilLogger
TestPrepareOptionsDefaultsEmptyListenAddress
TestRunRejectsUnsafeListenAddressBeforeListening
TestRunReturnsListenError
TestRunServesAndGracefullyStopsOnContextCancellation
TestRunReturnsUnexpectedServeError
TestRunReturnsShutdownError
TestServerTimeoutConfiguration
```

Required assertions:

- nil context and nil logger fail before `listen` is called;
- `prepareOptions` changes an empty `Options.ListenAddress` to `DefaultListenAddress`, preserves a nonempty address and logger, and validates before `listen`;
- invalid bind fails before injected `listen` is called;
- listener errors retain `errors.Is` identity and operation context;
- an injected loopback listener on `127.0.0.1:0` serves requests, cancellation causes bounded shutdown, and successful cancellation returns `nil` rather than `context.Canceled`;
- unexpected serve errors are returned and logged;
- shutdown failure is returned and logged, with forced `Close` cleanup;
- `newHTTPServer` sets exactly a 5-second read-header timeout, 60-second idle timeout, and zero write timeout;
- the production `Run` path passes a 10-second shutdown timeout, while tests call `run` with short deterministic bounds;
- startup, requested/effective listener address, shutdown, serve errors, and shutdown errors are present as structured `slog` records.

Use injected listeners, channels, and contexts rather than sleeps or mutable package globals.

- [ ] **Step 6: Run lifecycle tests RED**

Run:

```bash
go test ./internal/server -run '^(TestPrepareOptions|TestRun|TestServerTimeoutConfiguration)' -count=1
```

Expected: FAIL because `Run`, `run`, `newHTTPServer`, timeout configuration, and graceful shutdown are not implemented.

- [ ] **Step 7: Implement the inert constructor and HTTP lifecycle**

The foundation handler is intentionally minimal:

```go
func newHandler(*slog.Logger) (http.Handler, error) {
    return http.NotFoundHandler(), nil
}
```

`prepareOptions` must reject a nil logger, copy `DefaultListenAddress` over an empty listen address, validate the effective address, and return the normalized copy. `Run` must:

1. reject nil context;
2. call `prepareOptions` before constructing a listener;
3. call `newHandler` before listening and return a wrapped construction error;
4. delegate to `run(ctx, normalizedOptions, handler, net.Listen, 10*time.Second)`.

`run` also rejects nil context and calls `prepareOptions` so tests and internal callers cannot bypass the same validation boundary.

`newHTTPServer` must return:

```go
&http.Server{
    Addr:              address,
    Handler:           handler,
    ReadHeaderTimeout: 5 * time.Second,
    IdleTimeout:       60 * time.Second,
    WriteTimeout:      0,
}
```

`run` must listen on `tcp`, log requested and actual addresses, call `Serve`, and wait for either the serve result or `ctx.Done()`. On cancellation, derive a fresh timeout from `context.Background()`, call `Shutdown`, call `Close` if shutdown fails, and return the wrapped shutdown error. Treat `http.ErrServerClosed` as normal; log and return every unexpected listener, serve, or shutdown error with operation context. Do not return caller cancellation after successful graceful shutdown.

- [ ] **Step 8: Run foundation GREEN and commit**

Run:

```bash
gofmt -w internal/server/*.go
go test ./internal/server -count=1
go test ./... -count=1
git diff --check
git status --short --untracked-files=all
git add internal/server/server.go internal/server/server_test.go internal/server/handler.go
git commit -m "feat: add server lifecycle foundation"
FOUNDATION_COMMIT=$(git rev-parse HEAD)
git status --short
```

Expected: focused and full tests pass, the commit subject is exact, and final status is empty. Return `FOUNDATION_COMMIT`, changed files, and red/green evidence to the parent without requesting a review.

### Task 3: Run the Web and Cobra Lanes in Parallel from the Foundation

**Files:** No source changes in this coordination task.

**Interfaces:** Both lanes consume the exact foundation API; neither changes it.

- [ ] **Step 1: Dispatch both isolated writers with native managed worktrees**

After Task 2 succeeds, the parent dispatches one web writer and one Cobra writer concurrently, each with `worktree:true` and the exact `FOUNDATION_COMMIT`. No manual worktree command is allowed.

- [ ] **Step 2: Prove each lane starts at the foundation commit**

Each lane may fast-forward its isolated managed branch, then must check exact identity:

```bash
git merge --ff-only "$FOUNDATION_COMMIT"
test "$(git rev-parse HEAD)" = "$FOUNDATION_COMMIT"
git status --short
```

Expected: exact foundation `HEAD` and empty status before editing.

- [ ] **Step 3: Enforce disjoint ownership**

The web writer may change only:

```text
go.mod
go.sum
internal/server/handler.go
internal/server/handler_test.go
internal/server/web/app.css
internal/server/web/datastar.js
internal/server/web/datastar.LICENSE
internal/server/web/datastar.PROVENANCE
internal/server/web/index.html
```

The Cobra writer may change only:

```text
cmd/root.go
cmd/root_test.go
cmd/server.go
```

Expected: neither lane touches `internal/server/server.go`, `internal/server/server_test.go`, `main.go`, or the other lane's ownership. No reviewer is dispatched for either lane.

### Task 4: Web Lane — Add the Embedded Offline Datastar UI with TDD

**Files:**
- Modify: `internal/server/handler.go`
- Create: `internal/server/handler_test.go`
- Create: `internal/server/web/index.html`
- Create: `internal/server/web/app.css`
- Create: `internal/server/web/datastar.js`
- Create: `internal/server/web/datastar.LICENSE`
- Create: `internal/server/web/datastar.PROVENANCE`
- Modify: `go.mod`
- Modify: `go.sum`

**Interfaces:** Preserve `func newHandler(logger *slog.Logger) (http.Handler, error)`. Embed exactly `web/*`; expose only the five approved GET routes.

- [ ] **Step 1: Write failing route, page, and embedding tests**

Add `httptest` coverage with these exact contracts:

```text
TestHandlerRoutes
TestHandlerRejectsUnsupportedMethods
TestInitialPageContainsInertPanels
TestIndexRequiresClickForStatusRefresh
TestAssetsAreEmbedded
TestRenderedPageHasNoExternalRuntimeAssets
```

`TestHandlerRoutes` must assert:

- `GET /`: `200`, `Content-Type: text/html; charset=utf-8`, title `Kanedias`, visible `Refresh status`, and initial `Not refreshed yet.`;
- `GET /healthz`: `200`, `Content-Type: text/plain; charset=utf-8`, exact body `ok\n`;
- `GET /ui/status`: `200`, `Content-Type` beginning `text/event-stream`, exactly one SDK-formatted `datastar-patch-elements` event containing `id="server-status"`, `role="status"`, and `Running`;
- `GET /assets/app.css`: `200`, `Content-Type: text/css; charset=utf-8`, nonempty body;
- `GET /assets/datastar.js`: `200`, `Content-Type: text/javascript; charset=utf-8`, nonempty body;
- unknown path: `404`.

`TestHandlerRejectsUnsupportedMethods` must send POST, PUT, PATCH, and DELETE to each approved path and require `405` without invoking any state-changing callback.

`TestInitialPageContainsInertPanels` must require both approved future-view panels in initial HTML:

```html
<section id="dashboard-panel" aria-labelledby="dashboard-heading">
  <h2 id="dashboard-heading">Dashboard</h2>
  <p>Dashboard view is not available in this scaffold.</p>
</section>
<section id="session-panel" aria-labelledby="session-heading">
  <h2 id="session-heading">Sessions</h2>
  <p>Session view is not available in this scaffold.</p>
</section>
```

It must also prove those sections contain no forms, links, buttons, Datastar action attributes, Incus data, or mutation controls. The only interactive control on the page is the status refresh button.

`TestIndexRequiresClickForStatusRefresh` must require exactly one `/ui/status` occurrence and this binding on the visible button:

```html
data-on:click="@get('/ui/status')"
```

It must reject automatic request attributes such as `data-init`, scripts that fetch status, and any status reference outside that click binding.

`TestAssetsAreEmbedded` changes the process working directory to an empty temporary directory and confirms all five routes still work. `TestRenderedPageHasNoExternalRuntimeAssets` checks rendered `src` and `href` values and rejects `http://`, `https://`, protocol-relative URLs, CDN hosts, and package-manager paths.

- [ ] **Step 2: Write failing middleware and failure tests**

Add:

```text
TestPanicRecoveryReturnsGeneric500
TestRequestLogging
TestStatusStreamHonorsCanceledRequest
TestHandlerRejectsNilLogger
TestHandlerParsesTemplatesAtConstruction
```

Required assertions:

- a route injected through the same middleware stack panics before writing; the client gets `500` and exact generic body `Internal Server Error\n`, never the panic value or stack;
- the panic value appears only in an error-level structured log;
- request logs contain method, path, status, duration, and remote address;
- panic recovery is inside request logging so the completed request record reports `500`;
- an already canceled `/ui/status` request returns promptly, emits no event, and does not loop;
- nil logger is rejected at handler construction;
- invalid template bytes passed through a private `parseTemplates(fsys fs.FS)` seam fail during construction rather than after listen.

Do not add assertions for CSP, `X-Content-Type-Options`, `Referrer-Policy`, or other general security headers.

- [ ] **Step 3: Run the web tests RED**

Run:

```bash
go test ./internal/server -run '^(TestHandler|TestInitialPage|TestIndex|TestAssets|TestRendered|TestPanic|TestRequest|TestStatus)' -count=1
```

Expected: FAIL because the foundation handler returns `404`, browser resources do not exist, and the router/middleware/SSE behavior is absent.

- [ ] **Step 4: Pin the exact Go dependencies**

Run:

```bash
go get github.com/go-chi/chi/v5@v5.2.3
go get github.com/starfederation/datastar-go@v1.2.2
go list -m github.com/go-chi/chi/v5 github.com/starfederation/datastar-go
```

Expected module lines:

```text
github.com/go-chi/chi/v5 v5.2.3
github.com/starfederation/datastar-go v1.2.2
```

- [ ] **Step 5: Vendor and verify the official browser artifact**

Create `internal/server/web`, then download from immutable official tag URLs:

```bash
mkdir -p internal/server/web
curl --fail --location --proto '=https' --tlsv1.2 \
  https://raw.githubusercontent.com/starfederation/datastar/refs/tags/v1.0.2/bundles/datastar.js \
  -o internal/server/web/datastar.js
curl --fail --location --proto '=https' --tlsv1.2 \
  https://raw.githubusercontent.com/starfederation/datastar/refs/tags/v1.0.2/LICENSE \
  -o internal/server/web/datastar.LICENSE
DATASTAR_SHA256=$(sha256sum internal/server/web/datastar.js | awk '{print $1}')
test "${#DATASTAR_SHA256}" -eq 64
```

The upstream file contains the MIT license text. Write `internal/server/web/datastar.PROVENANCE` with the actual UTC retrieval date and command-computed digest:

```bash
RETRIEVAL_DATE=$(date -u +%F)
cat > internal/server/web/datastar.PROVENANCE <<EOF
Project: Datastar
Official repository: https://github.com/starfederation/datastar
Version: v1.0.2
Release tag: v1.0.2
Release archive: https://github.com/starfederation/datastar/archive/refs/tags/v1.0.2.tar.gz
Bundle source: https://raw.githubusercontent.com/starfederation/datastar/refs/tags/v1.0.2/bundles/datastar.js
License source: https://raw.githubusercontent.com/starfederation/datastar/refs/tags/v1.0.2/LICENSE
Vendored path: internal/server/web/datastar.js
Retrieval date: ${RETRIEVAL_DATE}
License identifier: MIT
SHA-256: ${DATASTAR_SHA256}
Modification: Vendored unchanged for offline embedding.
EOF
grep -F "Retrieval date: ${RETRIEVAL_DATE}" internal/server/web/datastar.PROVENANCE
grep -F 'License identifier: MIT' internal/server/web/datastar.PROVENANCE
```

Verify the recorded bytes:

```bash
RECORDED_SHA256=$(sed -n 's/^SHA-256: //p' internal/server/web/datastar.PROVENANCE)
test "$RECORDED_SHA256" = "$DATASTAR_SHA256"
printf '%s  %s\n' "$RECORDED_SHA256" internal/server/web/datastar.js | sha256sum -c -
```

Expected: `internal/server/web/datastar.js: OK`.

- [ ] **Step 6: Implement the embedded router and middleware**

Embed and parse the flat resource directory:

```go
//go:embed web/*
var webFiles embed.FS
```

`newHandler` must reject a nil logger, parse `web/index.html` with `html/template` before returning, and build a Chi router. Use these private seams for deterministic tests while keeping `newHandler`'s approved signature unchanged:

```go
func parseTemplates(fsys fs.FS) (*template.Template, error)
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler
func recoverPanics(logger *slog.Logger) func(http.Handler) http.Handler
```

Install middleware in this order so request logging wraps recovery:

```go
router.Use(requestLogger(logger), recoverPanics(logger))
```

The panic test applies those same middleware functions to a panicking `http.HandlerFunc`; production routes and test recovery therefore use identical code. Register exactly:

```go
router.Get("/", serveIndex)
router.Get("/healthz", serveHealth)
router.Get("/ui/status", serveStatus)
router.Get("/assets/app.css", serveCSS)
router.Get("/assets/datastar.js", serveJavaScript)
```

Read assets from `webFiles`; do not mount a filesystem prefix. Set content types before writing. The health body is exactly `ok\n`. For status, return immediately if `r.Context().Err() != nil`; otherwise use the official API:

```go
sse := datastar.NewSSE(w, r)
err := sse.PatchElements(
    `<output id="server-status" role="status">Running</output>`,
)
```

Emit one event and return. Log non-cancellation send failures without attempting a second response.

Implement custom structured request logging and generic panic recovery around the router. Recovery must be inside request logging so the final `500` status is recorded. Request records include method, path, status, duration, and remote address. Error records retain underlying errors. Generic client responses never include panic values, stack traces, or internal errors.

- [ ] **Step 7: Implement the approved initial HTML and CSS**

`internal/server/web/index.html` must contain:

- a complete HTML document with title and heading `Kanedias`;
- only local stylesheet `/assets/app.css` and local module script `/assets/datastar.js`;
- a short description that this is the local Kanedias server shell;
- `<output id="server-status" role="status">Not refreshed yet.</output>`;
- one visible `<button type="button" data-on:click="@get('/ui/status')">Refresh status</button>`;
- the exact inert `dashboard-panel` and `session-panel` sections from Step 1;
- no forms, auth controls, Incus content, mutation controls, inline remote content, or automatic request behavior.

`internal/server/web/app.css` may style the shell, status region, button, and two panels, but must not import external resources.

- [ ] **Step 8: Run web GREEN, verify scope, and commit**

Run:

```bash
gofmt -w internal/server/handler.go internal/server/handler_test.go
go mod tidy
go test ./internal/server -count=1
go test ./... -count=1
go vet ./...
git diff --check
go list -m github.com/go-chi/chi/v5 github.com/starfederation/datastar-go
RECORDED_SHA256=$(sed -n 's/^SHA-256: //p' internal/server/web/datastar.PROVENANCE)
printf '%s  %s\n' "$RECORDED_SHA256" internal/server/web/datastar.js | sha256sum -c -
git diff --name-only "$FOUNDATION_COMMIT"...HEAD
```

Expected: tests and vet pass; versions are exactly v5.2.3 and v1.2.2; checksum reports `OK`; changed paths are only the web lane ownership list.

Use precise runtime/scope scans that intentionally exclude upstream license, provenance, vendored JavaScript, and test method names:

```bash
grep -nE 'https?://|//[^[:space:]]+' internal/server/web/index.html internal/server/web/app.css && exit 1 || test "$?" -eq 1
grep -nE 'data-init|fetch\(|XMLHttpRequest' internal/server/web/index.html && exit 1 || test "$?" -eq 1
grep -nE '\.(Post|Put|Patch|Delete)\(' internal/server/handler.go && exit 1 || test "$?" -eq 1
grep -nE 'incus|database/sql|Set-Cookie' internal/server/handler.go internal/server/web/index.html && exit 1 || test "$?" -eq 1
```

Expected: every focused scan has no match. Then commit:

```bash
git add go.mod go.sum internal/server/handler.go internal/server/handler_test.go internal/server/web
git commit -m "feat: add embedded Datastar web UI"
WEB_COMMIT=$(git rev-parse HEAD)
git status --short
```

Expected: exact commit subject and empty status. Return `WEB_COMMIT`, changed paths, checksum, and red/green evidence to the parent without requesting a review.

### Task 5: Cobra Lane — Add the Server Command and Signal-Aware Execution with TDD

**Files:**
- Create: `cmd/server.go`
- Modify: `cmd/root.go`
- Modify: `cmd/root_test.go`

**Interfaces:** Add only `runServer func(context.Context, server.Options) error`; implement `newServerCommand`, `execute`, and signal-aware `Execute` exactly as approved. Keep `main.go` unchanged.

- [ ] **Step 1: Write failing hierarchy and command tests**

Add or update:

```text
TestCommandHierarchyAndFlags
TestServerCommandRejectsPositionalArguments
TestServerCommandRejectsUnsafeListenAddressBeforeDelegation
TestServerCommandDelegates
TestServerCommandUsesDefaultListenAddress
TestExecuteContextPropagatesCancellationToServer
```

Required assertions:

- root children are `image`, `profile`, `proxy`, `sandbox`, `server`, and `workspace` without changing existing descendants or persistent flags;
- `server` has one local `--listen` flag with default `server.DefaultListenAddress` and accepts no positional arguments;
- unsafe listen values return validation errors before `runServer`;
- delegation occurs exactly once with the exact Cobra command context, explicit listen value, and nonnil logger;
- writing through `Options.Logger` reaches the command's exact stderr buffer as structured text;
- `loadConfig`, `ensureNetwork`, and all Incus-backed services are not called;
- the exact sentinel `runServer` error is returned;
- omitting `--listen` delegates `127.0.0.1:8080`;
- calling `execute` with an already canceled context causes `runServer` to observe that exact context and `context.Canceled`, proving the context path used by production `Execute`;
- `stubServices()` supplies a no-op `runServer` so existing tests remain isolated.

- [ ] **Step 2: Run Cobra tests RED**

Run:

```bash
go test ./cmd -run '^(TestCommandHierarchyAndFlags|TestServerCommand|TestExecuteContext)' -count=1
```

Expected: FAIL because the service field, server command, command registration, and `execute` helper do not exist.

- [ ] **Step 3: Implement `cmd/server.go`**

Construct:

```go
func newServerCommand(service services) *cobra.Command
```

with `Use: "server"`, a `Short` description of the local Kanedias web UI, `Args: cobra.NoArgs`, and a local string `--listen` flag defaulting to `server.DefaultListenAddress`. In `RunE`, validate before delegation, then construct:

```go
logger := slog.New(slog.NewTextHandler(command.ErrOrStderr(), nil))
return service.runServer(
    command.Context(),
    server.Options{
        ListenAddress: listenAddress,
        Logger:        logger,
    },
)
```

Do not load config, inspect CLI globals from handlers, or invoke any Incus-related service.

- [ ] **Step 4: Wire the service, signal context, and test helper**

In `cmd/root.go`:

- import `os`, `os/signal`, `syscall`, and `internal/server`;
- add `runServer func(context.Context, server.Options) error` to `services`;
- add `runServer: server.Run` to `realServices`;
- register `newServerCommand(service)` on the root;
- add the exact `execute` helper from “Approved Interfaces and File Map”;
- update `Execute` to use `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`, defer `stop`, and call `execute`.

Do not edit `main.go`; its existing call to `cmd.Execute` remains the production entry point.

- [ ] **Step 5: Run Cobra GREEN, prove `main.go` unchanged, and commit**

Run:

```bash
gofmt -w cmd/root.go cmd/root_test.go cmd/server.go
go test ./cmd -count=1
go test ./... -count=1
go vet ./...
git diff --check
git diff --exit-code "$FOUNDATION_COMMIT" -- main.go
git diff --name-only "$FOUNDATION_COMMIT"...HEAD
```

Expected: all checks pass; `main.go` has no diff; changed paths are exactly:

```text
cmd/root.go
cmd/root_test.go
cmd/server.go
```

Commit:

```bash
git add cmd/root.go cmd/root_test.go cmd/server.go
git commit -m "feat: add server command"
COBRA_COMMIT=$(git rev-parse HEAD)
git status --short
```

Expected: exact commit subject and empty status. Return `COBRA_COMMIT`, changed paths, and red/green evidence to the parent without requesting a review.

### Task 6: Integrate the Foundation, Web, and Cobra Commits Deterministically

**Files:** Combined output of Tasks 2, 4, and 5 only.

**Interfaces:** Preserve all approved interfaces and strict lane ownership.

- [ ] **Step 1: Dispatch one integration/fix writer natively**

The parent dispatches one writer with `worktree:true`. The writer fast-forwards its isolated managed branch to the exact foundation commit if needed:

```bash
git merge --ff-only "$FOUNDATION_COMMIT"
test "$(git rev-parse HEAD)" = "$FOUNDATION_COMMIT"
git status --short
```

Expected: exact foundation `HEAD`, empty status, and no manual worktree creation.

- [ ] **Step 2: Confirm disjoint lane changes**

Run with exact commit hashes from native handoffs:

```bash
git diff --name-only "$FOUNDATION_COMMIT".."$WEB_COMMIT"
git diff --name-only "$FOUNDATION_COMMIT".."$COBRA_COMMIT"
```

Expected: only the declared web ownership in the first output and only the three declared Cobra files in the second; no overlap.

- [ ] **Step 3: Cherry-pick web then Cobra**

Run:

```bash
git cherry-pick "$WEB_COMMIT"
git cherry-pick "$COBRA_COMMIT"
git log --oneline -3
```

Expected newest-first subjects:

```text
feat: add server command
feat: add embedded Datastar web UI
feat: add server lifecycle foundation
```

No merge commits or conflict-resolution edits are expected. If a conflict occurs, stop and report it rather than broadening file ownership.

### Task 7: Run Integrated Automated Verification and Precise Scope Checks

**Files:** No changes unless a verified defect is found later through the single final review.

- [ ] **Step 1: Verify formatting without creating a diff**

Run:

```bash
gofmt -w cmd/root.go cmd/root_test.go cmd/server.go internal/server/*.go
git diff --exit-code
git diff --check
```

Expected: formatting creates no diff and both Git checks pass.

- [ ] **Step 2: Run focused, full, race, vet, and build checks**

Run:

```bash
go mod download
go test ./internal/server -count=1
go test ./cmd -count=1
go test ./... -count=1
go test -race ./internal/server ./cmd -count=1
go vet ./...
mkdir -p .tmp
go build -trimpath -o .tmp/kanedias .
```

Expected: every command exits `0` and the binary exists at `.tmp/kanedias`.

- [ ] **Step 3: Verify exact dependency and browser pins**

Run:

```bash
test "$(go list -m -f '{{.Version}}' github.com/go-chi/chi/v5)" = "v5.2.3"
test "$(go list -m -f '{{.Version}}' github.com/starfederation/datastar-go)" = "v1.2.2"
RECORDED_SHA256=$(sed -n 's/^SHA-256: //p' internal/server/web/datastar.PROVENANCE)
test "${#RECORDED_SHA256}" -eq 64
printf '%s  %s\n' "$RECORDED_SHA256" internal/server/web/datastar.js | sha256sum -c -
```

Expected: both version assertions pass and checksum output is `internal/server/web/datastar.js: OK`.

- [ ] **Step 4: Run precise marker and scope scans**

Build the unfinished-marker expression without writing those marker words into this plan, then scan only project-authored source and HTML/CSS:

```bash
MARKERS=$(printf '\124\117\104\117|\124\102\104|\106\111\130\115\105|\130\130\130|\122\105\120\114\101\103\105\137\115\105')
if grep -nE "$MARKERS" \
  cmd/root.go cmd/root_test.go cmd/server.go \
  internal/server/server.go internal/server/server_test.go \
  internal/server/handler.go internal/server/handler_test.go \
  internal/server/web/index.html internal/server/web/app.css; then
  exit 1
fi
if grep -nE 'https?://|//[^[:space:]]+' internal/server/web/index.html internal/server/web/app.css; then
  exit 1
fi
if grep -nE 'data-init|fetch\(|XMLHttpRequest' internal/server/web/index.html; then
  exit 1
fi
if grep -nE '\.(Post|Put|Patch|Delete)\(' internal/server/handler.go; then
  exit 1
fi
if grep -nE 'incus|database/sql|Set-Cookie' cmd/server.go internal/server/server.go internal/server/handler.go internal/server/web/index.html; then
  exit 1
fi
```

Expected: no matches. These scans intentionally exclude the upstream license, provenance URLs, vendored JavaScript, and test method-name words that are valid evidence rather than runtime scope violations.

- [ ] **Step 5: Prove file scope and `main.go` preservation**

Run from the parent of the foundation commit range:

```bash
git diff --name-only "$FOUNDATION_COMMIT^"..HEAD
git diff --exit-code "$FOUNDATION_COMMIT^"..HEAD -- main.go
git status --short --untracked-files=all
```

Expected: only declared foundation, web, Cobra, and module files; no `main.go` diff. Status may show only ignored `.tmp` contents, never tracked or staged changes.

### Task 8: Run Local Curl, Method-Safety, Bind-Rejection, and Signal Smoke Tests

**Files:** No tracked changes.

- [ ] **Step 1: Start the integrated binary on loopback and wait for readiness**

Run:

```bash
rm -f .tmp/server.stdout .tmp/server.stderr
.tmp/kanedias server --listen 127.0.0.1:18080 \
  >.tmp/server.stdout 2>.tmp/server.stderr &
SERVER_PID=$!
trap 'kill "$SERVER_PID" 2>/dev/null || true' EXIT
READY=0
for attempt in $(seq 1 50); do
  if curl --fail --silent http://127.0.0.1:18080/healthz >/dev/null; then
    READY=1
    break
  fi
  sleep 0.1
done
test "$READY" -eq 1
test "$(curl --fail --silent http://127.0.0.1:18080/healthz)" = "ok"
```

Expected: readiness succeeds and health content is `ok\n` (command substitution removes the trailing newline). If port 18080 is occupied, select an unused loopback port, use it consistently, and record it in the handoff.

- [ ] **Step 2: Smoke the HTML, inert panels, and embedded assets**

Run:

```bash
curl --fail --silent http://127.0.0.1:18080/ | tee .tmp/index.html
grep -F 'Refresh status' .tmp/index.html
grep -F 'Not refreshed yet.' .tmp/index.html
grep -F 'id="dashboard-panel"' .tmp/index.html
grep -F 'id="session-panel"' .tmp/index.html
grep -F 'data-on:click="@get('\''/ui/status'\'')"' .tmp/index.html
curl --fail --silent http://127.0.0.1:18080/assets/app.css >/dev/null
curl --fail --silent http://127.0.0.1:18080/assets/datastar.js >/dev/null
```

Expected: all fixed strings are present and both local assets return success.

- [ ] **Step 3: Smoke the one-shot Datastar SSE response**

Run:

```bash
curl --fail --silent --no-buffer http://127.0.0.1:18080/ui/status | tee .tmp/status.sse
test "$(grep -c '^event: datastar-patch-elements' .tmp/status.sse)" -eq 1
grep -F 'server-status' .tmp/status.sse
grep -F 'Running' .tmp/status.sse
```

Expected: one patch-elements event targets `server-status` and reports `Running`.

- [ ] **Step 4: Smoke method safety and unknown-route behavior**

Run:

```bash
for method in POST PUT PATCH DELETE; do
  test "$(curl --silent --output /dev/null --write-out '%{http_code}' \
    --request "$method" http://127.0.0.1:18080/ui/status)" = "405"
done
test "$(curl --silent --output /dev/null --write-out '%{http_code}' \
  http://127.0.0.1:18080/not-found)" = "404"
```

Expected: every state-changing method is `405`; unknown GET is `404`.

- [ ] **Step 5: Prove SIGTERM graceful shutdown and logs**

Run:

```bash
kill -TERM "$SERVER_PID"
wait "$SERVER_PID"
test "$?" -eq 0
trap - EXIT
grep -E 'server.*(start|listen)' .tmp/server.stderr
grep -E 'server.*(stop|shutdown)' .tmp/server.stderr
grep -E 'method=.*GET.*path=.*/healthz.*status=200' .tmp/server.stderr
```

Expected: process exits `0`; startup/effective-address, request, and shutdown structured records are present.

- [ ] **Step 6: Prove wildcard binding is rejected and clean transient output**

Run:

```bash
if .tmp/kanedias server --listen 0.0.0.0:18081 \
  >.tmp/rejected.stdout 2>.tmp/rejected.stderr; then
  echo "unsafe bind unexpectedly succeeded" >&2
  exit 1
fi
grep -E 'loopback|local|listen' .tmp/rejected.stderr
rm -rf .tmp
git status --short --untracked-files=all
```

Expected: unsafe bind exits nonzero without serving; final status is empty.

### Task 9: Perform Exactly One Fresh-Context Final Review

**Files:** Review-only in this task.

**Interfaces:** Review the approved API and behavior without proposing new product scope.

- [ ] **Step 1: Confirm the review gate is ready**

The parent confirms Tasks 6–8 passed and that no task-level or lane review occurred. If verification or curl smoke failed, fix verification setup or return to the owning integration/fix writer before dispatching review.

- [ ] **Step 2: Dispatch one independent reviewer**

Dispatch exactly one fresh-context reviewer who did not author foundation, web, Cobra, or integration changes. Provide:

- `docs/superpowers/specs/2026-08-06-server-web-scaffold-design.md`;
- this plan;
- the exact foundation-through-integration commit range;
- focused/full/race/vet/build output;
- dependency and browser checksum evidence;
- curl, method, bind-rejection, and SIGTERM evidence.

Require review of every spec section: command contract; local-only validation; flat `internal/server/web` embedding; all five routes; initial neutral status; both inert panels; click-only refresh; official SDK/bundle pins; generic panic handling; request/error logging; timeout values; bounded shutdown; exact Cobra context propagation; no `main.go` change; full offline operation; no Incus/auth/database/state mutation; test adequacy; file ownership; and native-worktree compliance.

Findings must include severity, file, line, impact, and concrete remediation. The report must explicitly say `no findings` when no actionable issue exists.

- [ ] **Step 3: Enforce the single-review rule**

Expected: exactly one final review report exists. Do not request task reviews, lane reviews, a second opinion, or review of review fixes.

### Task 10: Disposition Review Findings with the Integration/Fix Writer

**Files:** Only files directly required by valid findings; no files when the reviewer reports no findings.

**Interfaces:** Do not alter approved exported signatures or scope without user approval.

- [ ] **Step 1: Record a disposition for every finding**

The same integration/fix writer records each finding as one of:

```text
accepted and fixed
rejected with concrete code/test evidence
escalated because it requires an unapproved product or architecture choice
```

Do not silently implement an unapproved choice.

- [ ] **Step 2: Use red/green TDD for each accepted behavioral finding**

For each accepted behavior defect:

1. add or strengthen the narrowest owned test;
2. run its exact focused command and record the expected/actual failure;
3. apply the smallest correction;
4. rerun the focused command and record PASS.

Documentation, attribution-byte, or build-script-only findings use the narrowest applicable deterministic validation instead of inventing a behavior test.

- [ ] **Step 3: Commit all valid fixes once, if needed**

If and only if accepted findings require changes:

```bash
git add -u
git diff --cached --name-only
git commit -m "fix: address server web scaffold review"
```

Before committing, compare the staged path list with the accepted dispositions and unstage any unrelated path. Expected: at most one review-fix commit. Do not launch another reviewer.

### Task 11: Reverify and Preserve the Native Integration Handoff

**Files:** No additional changes.

- [ ] **Step 1: Re-run complete verification after disposition**

Run whether or not fixes were needed:

```bash
gofmt -w cmd/root.go cmd/root_test.go cmd/server.go internal/server/*.go
git diff --exit-code
go test ./internal/server -count=1
go test ./cmd -count=1
go test ./... -count=1
go test -race ./internal/server ./cmd -count=1
go vet ./...
go build -trimpath -o /tmp/kanedias-server-verified .
git diff --check
```

Expected: every command passes and formatting creates no diff.

- [ ] **Step 2: Re-run affected artifact and smoke checks**

Always rerun the exact version and SHA-256 assertions from Task 7. If any server, handler, command, web resource, module, or test file changed during review disposition, rerun all precise scans in Task 7 and all smoke commands in Task 8.

Expected: exact pins, checksum, scope scans, routes, one-shot SSE, method safety, bind rejection, logging, and SIGTERM shutdown all pass.

- [ ] **Step 3: Capture the final native handoff evidence**

Run:

```bash
git branch --show-current
git rev-parse --show-toplevel
git log --reverse --oneline "$FOUNDATION_COMMIT^"..HEAD
git diff --stat "$FOUNDATION_COMMIT^"..HEAD
git status --short --untracked-files=all
git diff --cached --name-only
```

Expected: ordered implementation commits are foundation, web, Cobra, and optionally one review-fix commit; worktree status and staged-file list are empty.

The handoff reports the managed integration branch name, native handoff artifact, exact ordered commits, changed-file/stat summary, tests, race/vet/build, dependency and checksum evidence, curl smoke, review report and dispositions, and residual risks. Preserve the managed integration branch and artifact; do not promise persistence of its temporary filesystem path and do not merge into the user's checkout.

## Dependencies and Execution Graph

1. Task 1 must pass before source editing.
2. Task 2 is sequential and produces `FOUNDATION_COMMIT`.
3. Task 3 dispatches Tasks 4 and 5 in parallel from that exact commit with strict disjoint ownership.
4. Task 6 depends on successful web and Cobra commits and integrates web before Cobra.
5. Tasks 7 and 8 run in the integrated managed worktree.
6. Task 9 starts only after automated verification and curl smoke pass, and is the only review.
7. Task 10 uses the one integration/fix writer to disposition that review without a second reviewer.
8. Task 11 reverifies and preserves the managed integration branch and native handoff artifact.

```text
approved design + committed plan
              |
      sequential foundation
          /             \
  web/Datastar lane   Cobra lane
          \             /
       one integration writer
              |
 automated verification + curl smoke
              |
 exactly one fresh-context final review
              |
 same integration/fix writer dispositions
              |
 re-verification + preserved native handoff
```

## Final Acceptance Checklist

- [ ] Every approved design section maps to at least one task and validation command.
- [ ] Both approved inert panels appear in implementation instructions, handler tests, and curl smoke.
- [ ] Exported and private signatures remain consistent from foundation through Cobra and web lanes.
- [ ] All browser resources are below `internal/server/web` and embedded with `//go:embed web/*`.
- [ ] Chi is v5.2.3, datastar-go is v1.2.2, and the unchanged browser bundle is v1.0.2 with verified license/provenance/SHA-256.
- [ ] No CSP or unapproved general security-header requirement/test exists.
- [ ] No task-level or lane reviews occurred; exactly one final review follows integrated verification and curl smoke.
- [ ] Every writer used harness-native `worktree:true`; no manual worktree was created.
- [ ] `main.go` is unchanged and the canceled context reaches `runServer` through `execute`.
- [ ] File ownership and dependency order match the foundation → parallel lanes → integration graph.
- [ ] Focused/full/race/vet/build, precise scans, checksum, curl, method, bind, and SIGTERM checks pass.
- [ ] The integration branch, ordered commits, native handoff artifact, clean status, and residual risks are reported.
