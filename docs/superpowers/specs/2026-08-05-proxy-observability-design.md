# Proxy Observability Design

## Goal

Add opt-in, privacy-conscious request logging and Prometheus metrics without changing proxy behavior when observability flags are absent.

## Command-line interface

- `-request-log` enables structured request and CONNECT logs on stderr using Go's standard `log/slog` text format.
- `-metrics-listen <address>` starts a separate HTTP server exposing `/metrics`. An empty value disables metrics; a typical local value is `127.0.0.1:9090`.
- Both features are disabled by default. The proxy listener and metrics listener remain separate.

## Request logging

Completed intercepted/plain HTTP requests produce one `proxy request` record with session, normalized route, method, host, escaped path, status, outcome, and duration to response headers. CONNECT decisions produce one `proxy connect` record with session, target, normalized route, and action (`mitm` or `tunnel`). Upstream failures log at warning level.

Logs never include query strings, request or response headers, authorization values, cookies, bodies, or raw transport errors. Upstream failures use the bounded `error_class=round_trip` field. Host and path are logging fields only and never Prometheus labels.

## Metrics

A private Prometheus registry exposes standard Go/process collectors plus:

- `kanedias_proxy_requests_total{route,method,status_class,outcome}`
- `kanedias_proxy_request_duration_seconds{route,method}`
- `kanedias_proxy_requests_in_flight{route}`
- `kanedias_proxy_connect_total{route,action}`
- `kanedias_proxy_upstream_errors_total{route}`
- `kanedias_proxy_credentials_total{provider,result}`

Routes are bounded to `github`, `anthropic`, `openai`, `openai_codex`, and `passthrough`. Methods are bounded to common HTTP methods plus `OTHER`; status classes and outcomes are similarly bounded. Credential results are `injected` or `missing`. No target host, path, instance, account, or token is used as a metric label.

## Integration

An optional observer is registered before credential injection. It stores per-request start state in `goproxy.ProxyCtx`, wraps the request's upstream round trip to account for errors that bypass response filters, and finishes successful or synthetic requests in a response filter. CONNECT decisions and credential outcomes are recorded at their existing decision points. Existing tests continue using the unobserved `newProxy` wrapper.

The metrics server uses `promhttp` on a dedicated `http.Server` with a header timeout. Failure of either the proxy or metrics listener is fatal and reported through structured startup logging.

## Verification

Tests cover log field presence, query/header/token omission, bounded route/method classification, successful/synthetic/error request metrics, in-flight balance, CONNECT actions, credential outcomes, and Prometheus HTTP exposition. Existing proxy and Incus tests remain green.
