# Kanedias Server Web Scaffold Design

## Goal and Scope

Add `kanedias server` to the existing Cobra command hierarchy. The command serves a local-only web shell that proves the complete browser-to-Chi-to-Datastar path without introducing application state or Incus behavior.

The initial page contains only a Kanedias title, a process/server status region with a visible `Refresh status` control, and clearly inert placeholder panels for future dashboard and session views. The status describes this HTTP process, not Incus or sandbox health.

## Command and Local-Only Boundary

`kanedias server` accepts no positional arguments and adds one local flag:

```text
--listen 127.0.0.1:8080
```

The command preserves all existing commands and persistent flags. It passes its Cobra context, selected listen address, stderr logger, and explicit dependencies into the server package rather than reading CLI globals from HTTP handlers.

There is no authentication in this scaffold because the listener is restricted to the local machine. Before opening a socket, address validation uses `net.SplitHostPort` and accepts only:

- the hostname `localhost`, case-insensitively;
- an IPv4 address for which `net.IP.IsLoopback` is true;
- an IPv6 address for which `net.IP.IsLoopback` is true, using normal bracketed host/port syntax such as `[::1]:8080`.

Empty hosts, wildcard addresses, non-loopback IPs, and other hostnames are rejected. Arbitrary hostnames are not accepted by resolving them and checking their current result; this avoids turning DNS into part of the security boundary. The port may be any value accepted by `net.Listen`, including `0` for tests.

## Package and Asset Boundaries

`cmd/server.go` owns the Cobra command, flag binding, logger construction, and delegation. `internal/server` owns listen-address validation, router construction, handlers, middleware, HTTP lifecycle, and graceful shutdown. Its constructors accept explicit options and dependencies, including a `*slog.Logger`; there is no package-global server, logger, or CLI configuration. This injection seam permits later Incus-backed services without coupling handlers to Cobra or constructing an Incus client in this scaffold.

Browser resources live below `internal/server/web` and are compiled into the Go binary with `go:embed`. They include the HTML template, CSS, the vendored Datastar browser bundle, and its upstream license/attribution. The resulting executable needs no runtime asset directory.

The server uses `html/template`. Templates are parsed when the server is constructed so invalid embedded templates fail before listening. Templ is deliberately deferred: this shell has no reusable typed component model that justifies adding frontend code generation. It can be reconsidered when multiple reusable, typed UI components exist.

## HTTP Interface

A Chi router exposes exactly the initial read-only surface:

| Route | Response |
| --- | --- |
| `GET /` | Full HTML shell rendered with `html/template`. |
| `GET /healthz` | `200 text/plain` with `ok\n`, indicating only that the server process can answer HTTP. |
| `GET /ui/status` | Datastar `text/event-stream` response containing a patch for the page's process-status element. |
| `GET /assets/app.css` | Embedded stylesheet with an explicit CSS content type. |
| `GET /assets/datastar.js` | Embedded Datastar browser bundle with an explicit JavaScript content type. |

Unknown paths and unsupported methods use Chi's normal not-found/method-not-allowed behavior. The HTML loads only the embedded asset URLs. Its `server-status` element begins in a neutral idle state. The page does not request status automatically on load; activating the visible `Refresh status` control invokes a Datastar action that requests `GET /ui/status`. The endpoint emits a single SDK-generated element patch replacing that same ID with a running state, then completes the response. This one-shot exchange proves browser, routing, SSE generation, and DOM patching without implying a persistent event model.

Handler failures return generic client-facing responses and never expose stack traces or internal error details. Detailed failures are recorded through the injected logger.

## Datastar and Offline Delivery

The implementation pins the official `github.com/starfederation/datastar-go` SDK at `v1.2.2` in `go.mod`/`go.sum` and uses its `datastar.NewSSE` and `PatchElements` APIs rather than hand-formatting the wire protocol. It checks in the self-hosted `bundles/datastar.js` browser bundle from the official `starfederation/datastar` GitHub repository at the `v1.0.2` release under `internal/server/web`, together with the corresponding upstream license. An adjacent attribution/provenance note records the official repository, the `v1.0.2` release tag, the official release archive URL, the vendored file path, and the vendored file's SHA-256 digest.

All HTML, CSS, JavaScript, license, and attribution files are embedded into one Go executable. There is no CDN, npm installation, package lockfile, bundler, generated frontend source, web-asset fetch during build or startup, or runtime static-file directory. Vendoring the browser artifact is distinct from generating application frontend code.

## Lifecycle, Logging, and Failure Handling

Every `internal/server` component receives and uses the same injected `*slog.Logger`. Production constructs a `slog` text handler on stderr. Structured records cover startup, the effective listen address, shutdown, requests, recovered panics, serve failures, and shutdown failures. Request records include method, path, status, duration, and remote address; error records retain the underlying error. Secrets and full response bodies are not logged.

The `http.Server` sets a 5-second read-header timeout and a 60-second idle timeout. It intentionally does not impose a whole-response write timeout that would make future SSE streams invalid. Chi request logging and panic recovery are configured so recovery details flow through the injected `slog.Logger`; a recovered panic produces a generic HTTP 500 response.

Serving and cancellation are coordinated explicitly. Cancellation of the command context stops acceptance of new work and calls `http.Server.Shutdown` with a fresh 10-second bound. A normal `http.ErrServerClosed` during this path is not reported as a serve failure. Timeout, listener, unexpected serve, and shutdown errors are returned with operation context after being logged. Production CLI signal handling cancels the command context for interrupt or termination signals, while tests can cancel it directly.

## State Model and Future Seam

This scaffold has no database or persistence layer. It performs no Incus calls and owns no session, sandbox, workspace, or proxy state. Future dashboard state remains Incus-owned, consistent with the session design: an injected service can query Incus and translate its resources and metadata into view models without making the web package authoritative state storage.

A later Incus-backed dashboard may replace placeholder panels with read-only resource summaries. A later session UI may use long-lived Datastar SSE for session event streaming, with authentication, authorization, reconnect, and replay designed at that time. Neither future behavior is implied by the one-shot process-status patch.

## Testing and Verification Contract

Automated coverage includes:

- Cobra hierarchy, default and overridden `--listen` values, no-argument enforcement, and delegation with the command context and injected service;
- table-driven acceptance of `localhost`, IPv4 loopback, and IPv6 loopback addresses, plus rejection of empty, wildcard, malformed, hostname, and non-loopback binds;
- `httptest` coverage for the HTML shell, visible manual refresh control and its `GET /ui/status` Datastar action, embedded CSS and JavaScript, plain health response, not-found behavior, and a Datastar SSE status patch with the expected content type and target element;
- a listener-level test using a loopback ephemeral port that cancels the serving context and proves bounded graceful shutdown;
- confirmation that handler and recovery failures produce generic client errors while retaining structured server logs.

Repository verification is:

```text
go test ./... -count=1
go vet ./...
go build ./...
git diff --check
```

A local smoke test starts the built binary on a loopback address, curls `/`, `/healthz`, `/ui/status`, and both asset routes, then cancels the process and confirms clean shutdown. Browser automation is not added for this static shell; SSE semantics are checked at the HTTP level.

## Explicit Non-Goals

This design does not add:

- authentication, authorization, users, or remote/non-loopback serving;
- TLS or reverse-proxy deployment support;
- Incus discovery, calls, lifecycle operations, or health checks;
- session creation, control, reconnect, replay, or live Pi event streaming;
- a database, migrations, persistence, cache, or server-owned application state;
- state-changing HTTP routes, forms, or APIs;
- proxy supervision or integration with the credential proxy;
- Templ, npm, a bundler, generated frontend code, a CDN, or runtime assets;
- browser automation or a general UI component framework.
