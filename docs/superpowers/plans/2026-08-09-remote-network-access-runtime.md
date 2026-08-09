# Remote Network Access Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Kanedias listen successfully on `0.0.0.0:8080`, advertise `http://steven-desktop:8080`, and trust reachable private-network clients by default while retaining opt-in session and same-origin protections.

**Architecture:** Configuration separates the advertised hostname from the socket bind address. Listener validation accepts literal IP addresses, handler construction derives an advertised authority from the configured hostname plus the effective listener port, and `require_session` conditionally installs the complete authentication/write-boundary layer.

**Tech Stack:** Go, Cobra, TOML, `net/http`, GNU Make, Git, GitHub CLI

## Global Constraints

- Makefile `BIND` remains defaulted to `0.0.0.0` and `PORT` to `8080`.
- `[server].hostname` is optional and the local value is exactly `steven-desktop`.
- Omitted `require_session` resolves to `false`.
- `require_session=false` installs neither bootstrap/session authentication nor the Host/Origin/Fetch-Site/Content-Type write boundary.
- `require_session=true` retains both session authentication and same-origin write protections.
- Bind hostnames other than `localhost` remain unsupported; any literal IPv4 or IPv6 address is accepted.
- The egress proxy address and behavior remain unchanged.
- Trusted-network mode intentionally grants full control to every client able to reach the server port.

## File Structure

- Modify `internal/config/server.go`: add and validate the advertised hostname and change the session-auth default.
- Modify `internal/config/server_test.go`: cover hostname resolution/validation and the false auth default.
- Modify `config.toml`: set the local advertised hostname.
- Modify `internal/server/server.go`: accept literal network bind IPs and derive advertised authority.
- Modify `internal/server/server_test.go`: cover validation and advertised-authority behavior.
- Modify `cmd/server.go`: describe `--listen` as a bind address.
- Modify `cmd/root_test.go`: cover wildcard CLI delegation and invalid bind addresses.
- Modify `internal/server/security.go`: emit absolute bootstrap URLs and support secure wildcard/configured-hostname boundaries.
- Modify `internal/server/security_test.go`: cover absolute URLs and wildcard same-origin behavior.
- Modify `internal/server/handler.go`: emit the Web UI URL and gate the complete security layer.
- Modify `internal/server/handler_test.go`: cover trusted-network bypass and authenticated protection.

---

### Task 1: Resolve Advertised Hostname and Trusted-Network Defaults

**Files:**
- Modify: `internal/config/server.go`
- Modify: `internal/config/server_test.go`
- Modify: `config.toml`

**Interfaces:**
- Produces: `ServerConfig.Hostname string` from TOML key `server.hostname`.
- Produces: `ResolvedServerConfig.Hostname string` and `ResolvedServerConfig.RequireSession bool`.
- Consumes: plain DNS hostname values without a scheme, port, path, whitespace, empty labels, or invalid label-edge hyphens.

- [ ] **Step 1: Add failing tests for hostname resolution and the false auth default**

Append these tests:

```go
func TestServerConfigResolveDefaultsToTrustedNetworkMode(t *testing.T) {
	resolved, err := (ServerConfig{}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.RequireSession {
		t.Fatal("RequireSession = true, want false when omitted")
	}
	if resolved.Hostname != "" {
		t.Fatalf("Hostname = %q, want empty fallback", resolved.Hostname)
	}
}

func TestServerConfigResolveAdvertisedHostname(t *testing.T) {
	requireSession := true
	resolved, err := (ServerConfig{
		Hostname: "steven-desktop", RequireSession: &requireSession,
	}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Hostname != "steven-desktop" {
		t.Fatalf("Hostname = %q, want steven-desktop", resolved.Hostname)
	}
	if !resolved.RequireSession {
		t.Fatal("RequireSession = false, want explicit true preserved")
	}
}

func TestServerConfigResolveRejectsInvalidHostname(t *testing.T) {
	for _, hostname := range []string{
		"http://steven-desktop", "steven-desktop:8080", "steven/desktop",
		"two words", "-steven", "steven-", "steven..desktop",
	} {
		t.Run(hostname, func(t *testing.T) {
			_, err := (ServerConfig{Hostname: hostname}).Resolve()
			if err == nil || !strings.Contains(err.Error(), "server hostname") {
				t.Fatalf("Resolve() error = %v, want server hostname error", err)
			}
		})
	}
}
```

In the TOML fixture for `TestServerConfigResolveParsesQuotedDurations`, add:

```toml
hostname = "steven-desktop"
require_session = true
```

and assert that `resolved.Hostname == "steven-desktop"` and `resolved.RequireSession == true` after loading. Also add these assertions to `TestServerConfigResolveDefaultDurations`:

```go
if resolved.RequireSession {
	t.Error("RequireSession = true, want false when omitted")
}
if resolved.Hostname != "" {
	t.Errorf("Hostname = %q, want empty fallback", resolved.Hostname)
}
```

- [ ] **Step 2: Run the focused config tests and verify RED**

```bash
go test ./internal/config -run 'TestServerConfigResolve(Default|Advertised|RejectsInvalid)' -v
```

Expected: compilation fails because `Hostname` does not exist, and/or the auth-default assertion fails because omission currently resolves to true.

- [ ] **Step 3: Implement hostname fields, validation, and false auth default**

Add `Hostname string \`toml:"hostname"\`` to `ServerConfig` and `Hostname string` to `ResolvedServerConfig`. Add a focused helper that accepts empty values and validates plain DNS labels:

```go
func validateServerHostname(hostname string) error {
	if hostname == "" {
		return nil
	}
	if len(hostname) > 253 || strings.ContainsAny(hostname, ":/\\?#[]@ \t\r\n") {
		return fmt.Errorf("server hostname %q must be a plain DNS hostname", hostname)
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("server hostname %q contains an invalid DNS label", hostname)
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
				(char < '0' || char > '9') && char != '-' {
				return fmt.Errorf("server hostname %q contains an invalid DNS label", hostname)
			}
		}
	}
	return nil
}
```

At the beginning of `Resolve`, call the helper and return its error. Initialize `requireSession := false`, preserve explicit pointer overrides, and copy `Hostname: c.Hostname` into the resolved struct.

Set the tracked local configuration to:

```toml
[server]
hostname = "steven-desktop"
# Trusted private-network console: skip bootstrap/session auth and the browser
# write boundary. Set true to require both layers.
require_session = false
```

- [ ] **Step 4: Run focused and package config tests**

```bash
go test ./internal/config -run 'TestServerConfigResolve' -v
go test ./internal/config
```

Expected: all config tests pass.

- [ ] **Step 5: Commit the configuration behavior**

```bash
git add internal/config/server.go internal/config/server_test.go config.toml
git commit -m "feat(config): add advertised server hostname"
```

Expected: one commit containing hostname configuration, validation, the auth-default change, and local config.

---

### Task 2: Accept Wildcard and Private IP Bind Addresses

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`
- Modify: `cmd/server.go`
- Modify: `cmd/root_test.go`

**Interfaces:**
- Consumes: bind strings in `host:port` form.
- Produces: `ValidateListenAddress(address string) error`, accepting `localhost` or any literal IP and rejecting unsupported bind hostnames or malformed ports.
- Produces: Cobra `--listen` usage text describing a bind address.

- [ ] **Step 1: Change server validation tests first**

Move these addresses into the accepted table:

```go
"0.0.0.0:8080",
"[::]:8080",
"192.168.1.2:8080",
"8.8.8.8:8080",
```

Keep these rejected cases:

```go
"",
":8080",
"example.com:8080",
"127.0.0.1",
"127.0.0.1:http",
"127.0.0.1:-1",
"127.0.0.1:65536",
```

Replace `TestRunRejectsUnsafeListenAddressBeforeListening` with:

```go
func TestRunAcceptsNetworkListenAddressBeforeListening(t *testing.T) {
	listenErr := errors.New("listen sentinel")
	called := false
	listen := func(network, address string) (net.Listener, error) {
		called = true
		if network != "tcp" || address != "0.0.0.0:8080" {
			t.Fatalf("listen(%q, %q), want tcp, 0.0.0.0:8080", network, address)
		}
		return nil, listenErr
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := run(
		context.Background(),
		Options{ListenAddress: "0.0.0.0:8080", Logger: logger},
		http.NotFoundHandler(), listen, time.Millisecond,
	)
	if !called {
		t.Fatal("network listener was not called")
	}
	if !errors.Is(err, listenErr) {
		t.Fatalf("run error = %v, want listen sentinel", err)
	}
}
```

- [ ] **Step 2: Change CLI tests first**

Update `TestServerCommandRejectsUnsafeListenAddressBeforeDelegation` to reject only `:8080` and `example.com:8080`. Change `TestServerCommandDelegates` to pass and expect `0.0.0.0:9090`. Add this flag-description assertion near the existing default assertion:

```go
if !strings.Contains(listenFlag.Usage, "bind") {
	t.Errorf("server listen usage = %q, want bind-address wording", listenFlag.Usage)
}
```

- [ ] **Step 3: Run focused tests and verify RED**

```bash
go test ./internal/server -run 'TestValidateListenAddress|TestRunAccepts' -v
go test ./cmd -run 'TestServerCommand(RejectsUnsafeListenAddressBeforeDelegation|Delegates)$|TestCommandTree' -v
```

Expected: wildcard/private address acceptance and delegation fail under the loopback-only validator; the help assertion fails under “local address” wording.

- [ ] **Step 4: Remove only the loopback policy from validation**

Retain split-host/port and numeric-port checks. Replace the IP policy with:

```go
if strings.EqualFold(host, "localhost") {
	return nil
}
if net.ParseIP(host) == nil {
	return fmt.Errorf("validate listen address %q: host must be localhost or an IP address", address)
}
return nil
```

Change the Cobra flag description to:

```go
command.Flags().StringVar(&listenAddress, "listen", listenAddress, "bind address for the Kanedias web UI")
```

- [ ] **Step 5: Run focused tests and full affected packages**

```bash
go test ./internal/server -run 'TestValidateListenAddress|TestRunAccepts' -v
go test ./cmd -run 'TestServerCommand|TestCommandTree' -v
go test ./internal/server ./cmd
```

Expected: all affected tests pass.

- [ ] **Step 6: Commit bind-address support**

```bash
git add internal/server/server.go internal/server/server_test.go cmd/server.go cmd/root_test.go
git commit -m "feat(server): allow network bind addresses"
```

Expected: one commit limited to bind validation, its tests, and CLI wording.

---

### Task 3: Advertise the Configured Hostname and Absolute URLs

**Files:**
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`
- Modify: `internal/server/handler.go`
- Modify: `internal/server/handler_test.go`
- Modify: `internal/server/security.go`
- Modify: `internal/server/security_test.go`

**Interfaces:**
- Produces: `advertisedAddress(effectiveAddress, configuredHostname string) (string, error)`.
- Changes: `newHandlerWithOptions` receives an advertised authority instead of treating the effective listener address as the public authority.
- Changes: `newCapabilityStore(random io.Reader, output io.Writer, advertisedAddress string)` emits an absolute bootstrap URL.
- Produces operator output lines `Web UI: http://<authority>/` and, when enabled, `Bootstrap URL: http://<authority>/bootstrap?capability=...`.

- [ ] **Step 1: Add failing advertised-address tests**

Add table-driven coverage in `internal/server/server_test.go`:

```go
func TestAdvertisedAddressUsesConfiguredHostnameAndEffectivePort(t *testing.T) {
	tests := []struct {
		effective string
		hostname  string
		want      string
	}{
		{"0.0.0.0:8080", "steven-desktop", "steven-desktop:8080"},
		{"127.0.0.1:9090", "", "127.0.0.1:9090"},
		{"[::]:8080", "steven-desktop", "steven-desktop:8080"},
	}
	for _, tt := range tests {
		got, err := advertisedAddress(tt.effective, tt.hostname)
		if err != nil || got != tt.want {
			t.Errorf("advertisedAddress(%q, %q) = %q, %v; want %q", tt.effective, tt.hostname, got, err, tt.want)
		}
	}
}
```

- [ ] **Step 2: Add failing absolute operator URL tests**

Update `newCapabilityStore` test calls to pass `"steven-desktop:8080"`. Make `TestBootstrapPrintsTokenOnce` also assert:

```go
if !strings.HasPrefix(output, "Bootstrap URL: http://steven-desktop:8080/bootstrap?capability=") {
	t.Fatalf("bootstrap output = %q, want absolute advertised URL", output)
}
```

Add this table-driven handler test:

```go
func TestHandlerPrintsAdvertisedURLs(t *testing.T) {
	for _, requireSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("require_session_%t", requireSession), func(t *testing.T) {
			var output bytes.Buffer
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			_, err := newHandlerWithOptions(
				logger, "steven-desktop:8080", &output, nil, context.Background(), requireSession,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), "Web UI: http://steven-desktop:8080/\n") {
				t.Fatalf("operator output = %q, want advertised Web UI URL", output.String())
			}
			hasBootstrap := strings.Contains(output.String(), "Bootstrap URL: http://steven-desktop:8080/bootstrap?capability=")
			if hasBootstrap != requireSession {
				t.Fatalf("bootstrap URL present = %t, want %t; output = %q", hasBootstrap, requireSession, output.String())
			}
		})
	}
}
```

- [ ] **Step 3: Run focused tests and verify RED**

```bash
go test ./internal/server -run 'TestAdvertisedAddress|TestBootstrapPrintsTokenOnce|TestHandlerPrints' -v
```

Expected: compilation fails because the advertised-address helper and revised capability/handler signatures do not exist.

- [ ] **Step 4: Implement advertised authority derivation**

Add:

```go
func advertisedAddress(effectiveAddress, configuredHostname string) (string, error) {
	host, port, err := net.SplitHostPort(effectiveAddress)
	if err != nil {
		return "", fmt.Errorf("derive advertised address from %q: %w", effectiveAddress, err)
	}
	if configuredHostname != "" {
		host = configuredHostname
	}
	return net.JoinHostPort(host, port), nil
}
```

In the production handler factory, derive the authority from `effectiveAddress` and `resolved.Hostname`; wrap failures with `construct advertised address`. Pass the result into `newHandlerWithOptions`.

- [ ] **Step 5: Implement absolute Web UI and Bootstrap output**

Use `net/url` for structured output:

```go
func absoluteHTTPURL(authority, path string, query url.Values) string {
	u := url.URL{Scheme: "http", Host: authority, Path: path}
	u.RawQuery = query.Encode()
	return u.String()
}
```

At handler construction, write `Web UI: <absolute-root-url>`. Change `newCapabilityStore` to receive the advertised authority and write its token in an absolute `/bootstrap` URL. Preserve token digest storage, no-store/referrer headers, and relative `/` redirect behavior.

Update all test/helper call sites with explicit advertised addresses. Use `DefaultListenAddress` for legacy test helpers that do not care about external access.

- [ ] **Step 6: Run focused tests and affected packages**

```bash
go test ./internal/server -run 'TestAdvertisedAddress|TestBootstrap|TestHandlerPrints' -v
go test ./internal/server
```

Expected: absolute URL, fallback, token, and existing server tests pass.

- [ ] **Step 7: Commit advertised URL behavior**

```bash
git add internal/server/server.go internal/server/server_test.go internal/server/handler.go internal/server/handler_test.go internal/server/security.go internal/server/security_test.go
git commit -m "feat(server): advertise configured web hostname"
```

Expected: one cohesive commit for advertised authority propagation and operator URLs.

---

### Task 4: Gate the Complete Browser Security Layer

**Files:**
- Modify: `internal/server/handler.go`
- Modify: `internal/server/handler_test.go`
- Modify: `internal/server/security.go`
- Modify: `internal/server/security_test.go`

**Interfaces:**
- Consumes: `requireSession bool` and advertised authority.
- Produces: trusted-network handler mode without session or write-boundary middleware when false.
- Produces: authenticated mode with session middleware and an authority-aware write boundary when true.

- [ ] **Step 1: Add a failing trusted-network handler test**

Add this test:

```go
func TestHandlerTrustedNetworkModeBypassesBrowserSecurity(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := newHandlerWithOptions(
		logger, "steven-desktop:8080", io.Discard, nil, context.Background(), false,
	)
	if err != nil {
		t.Fatal(err)
	}

	get := httptest.NewRequest(http.MethodGet, "/", nil)
	get.Host = "other-private-host:8080"
	getResult := httptest.NewRecorder()
	handler.ServeHTTP(getResult, get)
	if getResult.Code != http.StatusOK {
		t.Fatalf("trusted-network GET status = %d, want 200", getResult.Code)
	}

	post := httptest.NewRequest(http.MethodPost, "/ui/sessions", strings.NewReader("not json"))
	post.Host = "other-private-host:8080"
	post.Header.Set("Origin", "http://different-private-host:8080")
	post.Header.Set("Sec-Fetch-Site", "cross-site")
	post.Header.Set("Content-Type", "text/plain")
	postResult := httptest.NewRecorder()
	handler.ServeHTTP(postResult, post)
	if postResult.Code != http.StatusNotFound {
		t.Fatalf("trusted-network POST status = %d, want downstream 404", postResult.Code)
	}
}
```

The downstream 404 proves all write-boundary checks were bypassed rather than returning 403 or 415.

- [ ] **Step 2: Add failing wildcard/configured-authority boundary tests**

Add these tests:

```go
func TestRequestBoundaryWildcardUsesActualRequestHost(t *testing.T) {
	boundary := newRequestBoundary("0.0.0.0:8080")
	handler := boundary.requireWriteBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	tests := []struct {
		name   string
		host   string
		origin string
		want   int
	}{
		{"matching private hostname", "steven-desktop:8080", "http://steven-desktop:8080", http.StatusOK},
		{"different origin", "steven-desktop:8080", "http://other-host:8080", http.StatusForbidden},
		{"wrong port", "steven-desktop:9090", "http://steven-desktop:9090", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/ui/sessions", nil)
			req.Host = tt.host
			req.Header.Set("Origin", tt.origin)
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

func TestRequestBoundaryConfiguredHostnameIsCanonical(t *testing.T) {
	boundary := newRequestBoundary("steven-desktop:8080")
	handler := boundary.requireWriteBoundary(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for _, tt := range []struct {
		host string
		want int
	}{
		{"steven-desktop:8080", http.StatusOK},
		{"192.168.1.10:8080", http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodPost, "/ui/sessions", nil)
		req.Host = tt.host
		req.Header.Set("Origin", "http://"+tt.host)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != tt.want {
			t.Errorf("host %q status = %d, want %d", tt.host, w.Code, tt.want)
		}
	}
}
```

- [ ] **Step 3: Run focused tests and verify RED**

```bash
go test ./internal/server -run 'TestHandlerTrustedNetwork|TestRequestBoundary(Wildcard|Configured)' -v
```

Expected: trusted mode returns a boundary error under current always-on middleware, and wildcard remote Host matching fails.

- [ ] **Step 4: Gate session and write middleware together**

Use no-op middleware by default. Only when `requireSession` is true:

```go
auth, err = newCapabilityStore(defaultRandom, operatorOutput, advertisedAddress)
if err != nil {
	return nil, fmt.Errorf("create capability store: %w", err)
}
sessionRequired = auth.requireSession
writeRequired = newRequestBoundary(advertisedAddress).requireWriteBoundary
```

Register `/bootstrap` only when `auth != nil`. Apply `sessionRequired` to protected reads and both `sessionRequired` and `writeRequired` to action writes. With `requireSession=false`, neither middleware performs checks.

- [ ] **Step 5: Make wildcard boundaries same-origin against the actual request Host**

Keep exact and loopback-alias Host behavior. Extend `boundaryHostMatches` so an unspecified expected IP accepts any non-empty Host using the expected port. Change origin validation to receive the request Host and require the Origin authority to equal that actual Host (case-insensitively) in addition to satisfying the expected-authority policy:

```go
func boundaryOriginMatches(origin, requestHost, expected string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return strings.EqualFold(u.Host, requestHost) && boundaryHostMatches(u.Host, expected)
}
```

This prevents `Host: steven-desktop:8080` with `Origin: other-host:8080` from passing under a wildcard listener.

- [ ] **Step 6: Run focused and complete server tests**

```bash
go test ./internal/server -run 'TestHandlerTrustedNetwork|TestRequestBoundary|TestRequireSession' -v
go test ./internal/server
```

Expected: trusted-network bypass, authenticated enforcement, wildcard matching, existing loopback aliases, and all server tests pass.

- [ ] **Step 7: Commit security-mode behavior**

```bash
git add internal/server/handler.go internal/server/handler_test.go internal/server/security.go internal/server/security_test.go
git commit -m "feat(server): make browser security opt in"
```

Expected: one commit gating the complete browser security layer and preserving secure wildcard behavior when enabled.

---

### Task 5: Verify, Review, Deliver, and Launch

**Files:**
- No planned source-file changes

**Interfaces:**
- Consumes: the completed feature branch and GitHub permissions.
- Produces: a reviewed and merged pull request, updated local `main`, and a live server at `0.0.0.0:8080` advertised as `http://steven-desktop:8080`.

- [ ] **Step 1: Run formatting and the complete local verification suite**

```bash
gofmt -w cmd/server.go cmd/root_test.go internal/config/server.go internal/config/server_test.go internal/server/server.go internal/server/server_test.go internal/server/handler.go internal/server/handler_test.go internal/server/security.go internal/server/security_test.go
git diff --check
make test
make -n run | grep -F -- 'server --listen 0.0.0.0:8080'
git status --short
```

Expected: formatting produces no remaining diff after staging decisions, all Go packages and 20 Node tests pass, the generated command binds `0.0.0.0:8080`, and only intentional files are modified or committed.

- [ ] **Step 2: Commit any formatting-only residue if present**

If `gofmt` changed tracked files after the task commits:

```bash
git add cmd/server.go cmd/root_test.go internal/config/server.go internal/config/server_test.go internal/server/server.go internal/server/server_test.go internal/server/handler.go internal/server/handler_test.go internal/server/security.go internal/server/security_test.go
git commit -m "style: format remote access changes"
```

If the tree is already clean, do not create an empty commit.

- [ ] **Step 3: Request independent read-only review**

Review the range from `origin/main` to feature `HEAD` against this plan and `docs/superpowers/specs/2026-08-09-remote-network-access-runtime-design.md`. Require explicit severity-ranked findings and a ready-to-merge verdict. Fix Critical and Important findings test-first, rerun `make test`, and commit accepted fixes before proceeding.

- [ ] **Step 4: Push and create the pull request**

```bash
git push -u origin feat/remote-network-access
gh pr create \
  --base main \
  --head feat/remote-network-access \
  --title "feat: enable trusted private-network access" \
  --body $'## Summary\n- allow wildcard and private-IP web server binds\n- add an advertised server hostname and trusted-network default\n- gate session and same-origin browser protections behind require_session\n- advertise absolute Web UI and bootstrap URLs\n\n## Validation\n- make test\n- focused config, server, security, and CLI tests\n- independent code review'
```

Expected: GitHub reports a pull-request URL.

- [ ] **Step 5: Wait for required checks and squash-merge**

```bash
pr_number="$(gh pr view --json number --jq .number)"
gh pr checks "$pr_number" --watch --interval 10
gh pr merge "$pr_number" --squash --delete-branch
```

Expected: all checks pass and the PR state becomes `MERGED`. If `gh` reports only a local worktree checkout conflict after the remote merge, verify remote PR state before retrying any merge.

- [ ] **Step 6: Update and verify local main**

From the original repository checkout:

```bash
git switch main
git pull --ff-only origin main
make test
git status --short --branch
```

Expected: local `main` equals `origin/main`, the merged suite passes, and the checkout is clean.

- [ ] **Step 7: Launch from merged main**

First confirm port ownership:

```bash
ss -ltnp '( sport = :8080 )' || true
```

If no unrelated listener exists:

```bash
runtime_dir="${XDG_RUNTIME_DIR:-/tmp}/kanedias-make-run"
mkdir -p "$runtime_dir"
nohup make run BIND=0.0.0.0 PORT=8080 >"$runtime_dir/run.log" 2>&1 </dev/null &
echo $! >"$runtime_dir/run.pid"
```

Do not terminate an unrelated process. If a known previous Kanedias `make run` owns port 8080, stop only that known process before relaunching.

- [ ] **Step 8: Verify binding, advertised URL, and unauthenticated reachability**

```bash
runtime_dir="${XDG_RUNTIME_DIR:-/tmp}/kanedias-make-run"
for attempt in 1 2 3 4 5 6 7 8 9 10; do
  if curl --fail --show-error --silent http://127.0.0.1:8080/ >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
ss -ltn '( sport = :8080 )' | grep -F -- ':8080'
grep -F -- 'requested_address=0.0.0.0:8080' "$runtime_dir/run.log"
grep -F -- 'Web UI: http://steven-desktop:8080/' "$runtime_dir/run.log"
curl --fail --show-error --silent http://127.0.0.1:8080/ >/dev/null
curl --fail --show-error --silent --resolve steven-desktop:8080:127.0.0.1 http://steven-desktop:8080/ >/dev/null
```

Expected: the requested listener is `0.0.0.0:8080`, the socket is present (Linux/Go may display the dual-stack wildcard as `*:8080` with effective address `[::]:8080`), the log advertises `http://steven-desktop:8080/`, and both direct and configured-hostname HTTP checks succeed without bootstrap authentication. Report the PR URL, merge commit, runtime log, PID file, and any hostname-resolution caveat for other machines.
