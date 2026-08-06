# Agent-Friendly GitHub Proxy Design

## Goal

Make anonymous web requests to `github.com` behave like ordinary unauthenticated traffic while preserving host-side GitHub credential injection for Git and GitHub API requests. When GitHub credentials are required but unavailable, return one concise, agent-readable error.

## Root Cause

The proxy currently treats every request to `github.com` as credentialed. After intercepting TLS, it removes any caller-provided authorization and requires a host GitHub credential before forwarding the request. Consequently, an anonymous request such as `curl https://github.com` receives the proxy's synthetic missing-credential response instead of GitHub's normal web response.

The proxy must continue intercepting TLS for `github.com`: the CONNECT request exposes only the target authority, so the proxy cannot determine whether the eventual HTTPS request is authenticated until after interception. The fix therefore changes request handling rather than CONNECT routing.

## GitHub Request Classification

The proxy will classify GitHub traffic as follows:

- `api.github.com` and `uploads.github.com` remain credentialed hosts. Every HTTPS request to either host requires and receives the host GitHub credential.
- A request to `github.com` with a non-empty `Authorization` header is credentialed traffic. The proxy removes the caller-provided value and installs HTTP Basic authentication using username `x-access-token` and the host GitHub credential. This preserves private Git clone, fetch, and push through clients configured with a sandbox placeholder credential.
- A request to `github.com` without an `Authorization` header is anonymous web traffic. The proxy forwards it without adding authorization and does not require a host GitHub credential.
- Existing HTTPS and request-authority validation remains in force for all intercepted provider hosts.
- Traffic for Anthropic, OpenAI, OpenAI Codex, and non-intercepted hosts is unchanged.

An anonymous `github.com` request still traverses the existing TLS interception because authenticated and anonymous HTTPS requests share the same CONNECT authority. "Pass through unchanged" means that its HTTP method, URL, headers, and body are forwarded without GitHub credential mutation or a synthetic missing-credential response.

## Missing-Credential Response

When `api.github.com`, `uploads.github.com`, or an authenticated `github.com` request requires a host GitHub credential and none is available, the proxy returns:

- status: `502 Bad Gateway`;
- content type: `text/plain`;
- body: `GitHub auth unavailable`.

The response intentionally omits internal details and remediation instructions. Missing-credential responses for other providers remain unchanged.

## Observability

Existing route, CONNECT, request, and credential metrics remain structurally unchanged. Anonymous `github.com` traffic remains on the bounded `github` route, but it records neither an injected nor a missing GitHub credential result because credential handling was not requested. Authenticated GitHub traffic continues to record `injected` or `missing` as applicable.

Request logging remains privacy-safe and must not expose authorization values.

## Testing

Automated proxy tests will cover these externally observable behaviors:

1. An anonymous HTTPS request to `github.com` reaches the upstream server without an `Authorization` header when no host GitHub credential exists.
2. An authenticated HTTPS request to `github.com` replaces the caller credential with the host GitHub Basic credential.
3. An authenticated `github.com` request without a host credential receives status 502 and the exact body `GitHub auth unavailable`.
4. A GitHub API request without a host credential receives the same status and exact body.
5. Existing GitHub API, uploads, Git CLI, other-provider, authority-validation, tunneling, logging, and metrics tests continue to pass.

The defect test for anonymous `github.com` traffic must be written and observed failing before production code changes.

## Scope

This change does not remove TLS interception for `github.com`, alter GitHub credential discovery, add a GitHub login command, infer Git operations from paths or user agents, change non-GitHub provider errors, or modify Incus/profile configuration.
