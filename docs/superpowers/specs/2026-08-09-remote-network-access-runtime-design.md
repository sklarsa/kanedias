# Remote Network Access Runtime Design

## Goal

Make Kanedias usable from trusted private-network machines at `http://steven-desktop:8080` while listening on `0.0.0.0:8080` by default through the Makefile.

## Configuration

The `[server]` configuration gains an optional `hostname` string. The repository's local `config.toml` sets:

```toml
[server]
hostname = "steven-desktop"
require_session = false
```

`hostname` is the advertised/access hostname, not the socket bind address. Listener binding remains controlled by the Makefile's `BIND` and `PORT` variables. When present, it must be a plain DNS hostname, including single-label private names such as `steven-desktop`; schemes, ports, paths, whitespace, empty DNS labels, and labels with invalid leading or trailing hyphens are rejected during configuration resolution.

When `hostname` is omitted, generated or logged URLs fall back to the effective listener address. Kanedias will not infer a name using `os.Hostname()`, because system and container hostnames are not guaranteed to resolve from client machines.

An omitted `require_session` resolves to `false`. This changes the current resolver default from authenticated to trusted-network mode.

## Listen Address Validation

Listen addresses must retain a non-empty host and numeric port accepted by `net.SplitHostPort`. The host may be `localhost` or any literal IPv4/IPv6 address, including unspecified addresses such as `0.0.0.0` and `::` and private interface addresses. Arbitrary DNS names remain unsupported as bind addresses; operators should bind an IP and configure `server.hostname` separately.

Malformed addresses, empty hosts, nonnumeric or out-of-range ports, and unsupported bind hostnames continue to fail before the listener starts.

## Authentication and Request Boundaries

`require_session` controls the complete browser security layer:

- When `false`, Kanedias does not create bootstrap/session capabilities and does not apply the write boundary, including its Host, Origin, `Sec-Fetch-Site`, and JSON Content-Type checks. Any client that can reach the server can read the UI and invoke control actions.
- When `true`, Kanedias requires the bootstrap-issued session cookie and applies the existing same-origin write protections.

For authenticated wildcard listeners, the request boundary must support remote access correctly:

- If `server.hostname` is configured, its hostname plus the effective listener port is the expected request authority.
- If no hostname is configured and the listener host is unspecified, the request's Host is accepted when it uses the listener port, and any Origin must match that actual Host rather than merely matching another arbitrary host on the same port.
- For specific listener addresses, existing exact-address and loopback-alias behavior remains.

This preserves same-origin guarantees when security is enabled while making the checks entirely opt-in through `require_session`.

## Advertised URLs

After listening begins, Kanedias reports an absolute Web UI URL using `http`, the configured hostname when present, and the effective listener port. With the local configuration, it reports:

```text
Web UI: http://steven-desktop:8080/
```

When session authentication is enabled, the bootstrap URL is also absolute and uses the same advertised authority:

```text
Bootstrap URL: http://steven-desktop:8080/bootstrap?capability=...
```

When `hostname` is absent, these URLs use the effective listener address.

## Components

- `internal/config/server.go`: parse and resolve `server.hostname`; default `require_session` to false.
- `config.toml`: set the local advertised hostname to `steven-desktop` and retain trusted-network mode.
- `internal/server/server.go`: allow literal non-loopback and wildcard IP listen addresses; derive advertised authority after the listener exposes its effective port.
- `internal/server/handler.go`: install both session authentication and the request write boundary only when `require_session` is true.
- `internal/server/security.go`: generate absolute bootstrap URLs and enforce same-origin rules for configured hostnames and wildcard listeners when enabled.
- `cmd/server.go`: describe `--listen` as a bind address rather than a local-only address.

## Error Handling

Invalid listener syntax and unsupported bind hostnames return contextual validation errors before dependencies or listeners start. Invalid server configuration continues to fail during configuration resolution. URL construction uses structured host/port joining so IPv6 addresses remain valid.

## Validation

Tests will cover:

- omitted and explicit `require_session` values, including the new false default;
- omitted and explicit `server.hostname` values;
- acceptance of `0.0.0.0`, `::`, and private literal IP bind addresses;
- continued rejection of malformed addresses and unsupported bind hostnames;
- unauthenticated private-network requests and actions when `require_session=false`;
- bootstrap/session and Host/Origin enforcement when `require_session=true`;
- wildcard listeners with configured and request-derived hostnames;
- absolute advertised and bootstrap URLs;
- CLI delegation of wildcard bind addresses;
- the complete existing Go and Node test suites;
- a real launch on `0.0.0.0:8080`, local HTTP reachability, and listener inspection.

## Security Trade-off

Trusted-network mode is intentionally unauthenticated. With `require_session=false`, every device able to reach port 8080 can view sessions and invoke actions such as spawn, steer, interrupt, answer, and stop. Network segmentation and host firewall policy are the access controls for this mode.

## Delivery

The correction will be developed test-first in this worktree, independently reviewed, delivered through a second pull request, merged using the repository's allowed strategy, and verified from an updated local `main` before the service is launched.
