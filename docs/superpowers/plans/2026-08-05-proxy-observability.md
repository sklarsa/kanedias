# Proxy Observability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add opt-in structured proxy request logs and bounded-label Prometheus metrics on a separate listener.

**Architecture:** A focused observer owns request state, logging, and metrics while the proxy retains credential-routing responsibility. A private Prometheus registry prevents global collector collisions and is exposed only when `-metrics-listen` is configured.

**Tech Stack:** Go `log/slog`, `github.com/prometheus/client_golang` v1.24.1, goproxy

## Global Constraints

- Logging and metrics are disabled by default.
- Logs omit query strings, headers, credentials, cookies, and bodies.
- Metric labels are finite classifications; host and path never become labels.
- Existing `newProxy(ca, credentials)` behavior and tests remain compatible.
- Metrics use a separate listener and expose only `/metrics`.

---

### Task 1: Define metrics and bounded classifications

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Create: `proxy/observability.go`
- Create: `proxy/observability_test.go`

- [ ] Add failing table tests for host-to-route, method, and status-class classification with unknown values mapping to bounded fallbacks.
- [ ] Add failing registry tests for request count/duration/in-flight, CONNECT decisions, upstream errors, and credential outcomes.
- [ ] Run focused tests and confirm RED because observability types do not exist.
- [ ] Add Prometheus v1.24.1 and implement a private registry with Go/process collectors and the six `kanedias_proxy_*` metric families.
- [ ] Run focused tests until GREEN.

### Task 2: Instrument requests and privacy-safe logs

**Files:**
- Modify: `proxy/observability.go`
- Modify: `proxy/observability_test.go`
- Modify: `proxy/main.go`

- [ ] Add failing integration tests that exercise a successful intercepted request, a synthetic missing-credential response, an upstream error, and MITM/tunnel CONNECT decisions through an observed proxy.
- [ ] Assert structured log records include route/method/host/path/status/duration while excluding query values, authorization values, and bodies.
- [ ] Assert every started request reaches exactly one finish path and in-flight returns to zero.
- [ ] Keep `newProxy` as an unobserved wrapper and add an internal observed constructor.
- [ ] Register request observation before credential injection, wrap upstream round trips for error completion, finish responses in `OnResponse`, and record credential/CONNECT decisions at existing branches.
- [ ] Run all proxy tests until GREEN.

### Task 3: Add flags and metrics server

**Files:**
- Modify: `proxy/main.go`
- Modify: `proxy/observability_test.go`

- [ ] Add failing tests for Prometheus exposition through the dedicated handler and disabled-by-default flag defaults where practical.
- [ ] Add `-request-log` and `-metrics-listen`; construct a standard `slog` text logger and observer only when needed.
- [ ] Serve `/metrics` from `promhttp` on a separate `http.Server` with `ReadHeaderTimeout` and report listener failures through the main error channel.
- [ ] Verify help output documents both flags and metrics exposition contains custom and Go/process families.

### Task 4: Full verification and review

**Files:**
- Modify only if scoped verification or review exposes a defect.

- [ ] Run `gofmt`, `go test ./... -count=1`, `go vet ./...`, shell suites, and `git diff --check`.
- [ ] Run a local proxy/metrics smoke test and scrape `/metrics` without logging secrets.
- [ ] Request independent read-only review, fix all Critical/Important findings, then commit and push to `origin/main`.
