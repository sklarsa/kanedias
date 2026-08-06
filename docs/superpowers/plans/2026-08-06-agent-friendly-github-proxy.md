# Agent-Friendly GitHub Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pass anonymous `github.com` web requests through unchanged while retaining GitHub credential injection for authenticated Git and API traffic and returning a concise missing-auth error.

**Architecture:** Keep the existing host-based CONNECT interception so the proxy can inspect HTTPS requests. Classify `github.com` requests by whether the incoming request has an `Authorization` header before deleting credential headers; bypass credential handling for anonymous web requests, while preserving unconditional credential handling for `api.github.com` and `uploads.github.com`.

**Tech Stack:** Go 1.26.5, `github.com/elazarl/goproxy` v1.8.6, `net/http`, Go's `httptest` and `testing` packages

## Global Constraints

- `api.github.com` and `uploads.github.com` remain credentialed hosts for every HTTPS request.
- A `github.com` request with a non-empty `Authorization` header receives the host GitHub Basic credential.
- A `github.com` request without an `Authorization` header is forwarded without adding authorization and without requiring a host GitHub credential.
- `github.com` remains TLS-intercepted because classification occurs after CONNECT.
- Missing required GitHub credentials return status `502 Bad Gateway`, content type `text/plain`, and exact body `GitHub auth unavailable`.
- Missing-credential responses and request behavior for non-GitHub providers remain unchanged.
- The anonymous `github.com` regression test must be observed failing before production code changes.

---

## File Structure

- `internal/proxy/main_test.go` — add end-to-end HTTP proxy regression coverage for anonymous GitHub web traffic and exact GitHub missing-auth responses.
- `internal/proxy/proxy.go` — classify anonymous `github.com` requests before credential header mutation and provide the GitHub-specific synthetic error response.

### Task 1: Classify GitHub Traffic and Return the Agent-Friendly Error

**Files:**
- Modify: `internal/proxy/main_test.go`
- Modify: `internal/proxy/proxy.go`
- Test: `internal/proxy/main_test.go`

**Interfaces:**
- Consumes: existing `newProxy(tls.Certificate, credentials) *goproxy.ProxyHttpServer`, `observedMITMClient(*testing.T, string, []byte) *http.Client`, and `missingCredential(*http.Request, string) *http.Response` behavior.
- Produces: anonymous `github.com` pass-through based on the incoming `Authorization` header and `missingGitHubCredential(*http.Request) *http.Response` with the exact response contract from the spec.

- [ ] **Step 1: Add the failing anonymous GitHub regression test**

Append this test to `internal/proxy/main_test.go`:

```go
func TestProxyPassesAnonymousGitHubWebTraffic(t *testing.T) {
	ca, caPEM, _, err := generateCA("anonymous GitHub test")
	if err != nil {
		t.Fatal(err)
	}

	receivedAuthorization := make(chan string, 1)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuthorization <- r.Header.Get("Authorization")
		_, _ = io.WriteString(w, "github home")
	}))
	defer upstream.Close()

	handler := newProxy(ca, credentials{})
	handler.Tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	handler.Tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // local fake upstream
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()

	resp, err := observedMITMClient(t, proxyServer.URL, caPEM).Get("https://github.com/")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %q", resp.StatusCode, body)
	}
	if string(body) != "github home" {
		t.Fatalf("body = %q, want %q", body, "github home")
	}
	if authorization := <-receivedAuthorization; authorization != "" {
		t.Fatalf("Authorization = %q, want empty", authorization)
	}
}
```

This test uses the existing MITM client helper so it exercises CONNECT interception, request filtering, and the upstream transport rather than a private classification helper.

- [ ] **Step 2: Run the regression test and verify the current defect**

Run:

```bash
go test ./internal/proxy -run '^TestProxyPassesAnonymousGitHubWebTraffic$' -count=1 -v
```

Expected: FAIL with `status = 502, want 200; body = "GitHub credential is not configured"`. This proves the existing unconditional `github.com` credential requirement causes the reported behavior.

- [ ] **Step 3: Implement the minimal anonymous-request bypass**

In the credential injection request handler in `internal/proxy/proxy.go`, add the `github.com` anonymous check after HTTPS and authority validation but before any credential headers are deleted:

```go
		if host == "github.com" && req.Header.Get("Authorization") == "" {
			return req, nil
		}

		req.Header.Del("Authorization")
```

Do not change `interceptedHosts`, CONNECT routing, API host handling, provider handling, or observability labels.

- [ ] **Step 4: Run the regression and existing authenticated GitHub tests**

Run:

```bash
go test ./internal/proxy -run '^(TestProxyPassesAnonymousGitHubWebTraffic|TestProxyInjectsProviderCredentials|TestGitHubCLIWorksThroughProxy)$' -count=1 -v
```

Expected: PASS. `TestProxyInjectsProviderCredentials/GitHub_Git` proves a caller-provided `Authorization` header on `github.com` is still replaced with `x-access-token:<host token>` Basic authentication; `TestGitHubCLIWorksThroughProxy` proves GitHub API injection remains intact.

- [ ] **Step 5: Add failing exact-response tests for missing GitHub auth**

Append this test to `internal/proxy/main_test.go`:

```go
func TestProxyReturnsAgentFriendlyGitHubAuthError(t *testing.T) {
	ca, caPEM, _, err := generateCA("missing GitHub auth test")
	if err != nil {
		t.Fatal(err)
	}
	handler := newProxy(ca, credentials{})
	proxyServer := httptest.NewServer(handler)
	defer proxyServer.Close()
	client := observedMITMClient(t, proxyServer.URL, caPEM)

	tests := []struct {
		name          string
		url           string
		authorization string
	}{
		{name: "API", url: "https://api.github.com/user"},
		{name: "authenticated web", url: "https://github.com/owner/repository.git", authorization: "Basic sandbox-placeholder"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, test.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.authorization != "" {
				req.Header.Set("Authorization", test.authorization)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusBadGateway {
				t.Errorf("status = %d, want 502", resp.StatusCode)
			}
			if contentType := resp.Header.Get("Content-Type"); contentType != "text/plain" {
				t.Errorf("Content-Type = %q, want text/plain", contentType)
			}
			if string(body) != "GitHub auth unavailable" {
				t.Errorf("body = %q, want %q", body, "GitHub auth unavailable")
			}
		})
	}
}
```

The API case proves the established always-credentialed scope. The authenticated web case proves only explicit credential use on `github.com` requires the host token.

- [ ] **Step 6: Run the error-response test and verify it fails for the copy change**

Run:

```bash
go test ./internal/proxy -run '^TestProxyReturnsAgentFriendlyGitHubAuthError$' -count=1 -v
```

Expected: FAIL in both subtests with `body = "GitHub credential is not configured", want "GitHub auth unavailable"`, while status and content type already match.

- [ ] **Step 7: Implement the GitHub-specific missing-auth response**

Replace the GitHub calls to the generic helper in `internal/proxy/proxy.go`:

```go
return req, missingGitHubCredential(req)
```

Use that call in both the `"api.github.com", "uploads.github.com"` case and the `"github.com"` case. Add this helper immediately before the existing generic `missingCredential` function:

```go
func missingGitHubCredential(req *http.Request) *http.Response {
	return goproxy.NewResponse(req, goproxy.ContentTypeText, http.StatusBadGateway,
		"GitHub auth unavailable")
}
```

Leave `missingCredential` and every non-GitHub call to it unchanged.

- [ ] **Step 8: Format and run focused proxy verification**

Run:

```bash
gofmt -w internal/proxy/main_test.go internal/proxy/proxy.go
go test ./internal/proxy -run '^(TestProxyPassesAnonymousGitHubWebTraffic|TestProxyReturnsAgentFriendlyGitHubAuthError|TestProxyInjectsProviderCredentials|TestGitHubCLIWorksThroughProxy|TestObservedProxyMeasuresSyntheticMissingCredential)$' -count=1 -v
go test -race ./internal/proxy -count=1
```

Expected: all focused tests and the complete proxy package pass. The observability test confirms synthetic API failures still update bounded request and credential metrics.

- [ ] **Step 9: Run repository-wide verification**

Run:

```bash
go test ./... -count=1
go vet ./...
git diff --check
git status --short
```

Expected: all packages pass, `go vet` is silent, `git diff --check` is silent, and status lists only `internal/proxy/main_test.go` and `internal/proxy/proxy.go` as modified in addition to the already committed spec and plan history.

- [ ] **Step 10: Commit the implementation**

Run:

```bash
git add internal/proxy/main_test.go internal/proxy/proxy.go
git commit -m "fix: make GitHub proxy agent-friendly"
git status --short --branch
```

Expected: the implementation commit succeeds and the isolated worktree is clean.
