# Proxy CA Image Trust Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bake the persistent Kanedias proxy public CA into the published sandbox image so `curl`, `gh`, and HTTPS Git trust intercepted GitHub traffic in fresh Pi sessions.

**Architecture:** `image.Create` initializes the proxy package's default CA before connecting to Incus, reads only the public certificate into image build inputs, and uploads it to the image installer. The installer adds it to Debian's system trust bundle before image publication, while the sandbox profile stops mounting a host CA over the baked certificate.

**Tech Stack:** Go 1.24, Bash, Debian 13 `ca-certificates`, Incus 7, Git, GitHub CLI

## Global Constraints

- Never copy, upload, log, or embed the proxy CA private key.
- The public CA destination is exactly `/usr/local/share/ca-certificates/kanedias-proxy.crt` at mode `0644`.
- The system trust bundle remains `/etc/ssl/certs/ca-certificates.crt` and must be regenerated with `update-ca-certificates` before image publication.
- Keep `NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/kanedias-proxy.crt` and `SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt` in the sandbox profile.
- Remove the runtime `proxy-ca` disk device and host CA source path from the sandbox profile.
- Keep proxy lifecycle explicit; `proxy run` must not rebuild or republish images.
- Do not add SSH tunneling or Git SSH-to-HTTPS rewrites.
- Hermetic tests must isolate `XDG_CONFIG_HOME` and must not read or modify the operator's real proxy CA.
- Live validation may read private repositories and use `git push --dry-run`, but must not mutate GitHub.
- Preserve and clean up unrelated Incus sessions and resources.

---

### Task 1: Initialize and Upload the Public CA During Image Creation

**Files:**
- Modify: `internal/image/image.go`
- Test: `internal/image/image_test.go`

**Interfaces:**
- Consumes: `proxy.DefaultOptions() (proxy.Options, error)` and `proxy.InitCA(certPath, keyPath string) error`.
- Produces: `loadProxyCA() ([]byte, error)` and `buildInputs.proxyCA []byte`; image input `/root/assets/kanedias-proxy.crt` at mode `0644`.

- [ ] **Step 1: Write failing tests for public-CA loading, pre-connect failure, and upload privacy**

In `imageConfig`, isolate every image test from the operator's real configuration:

```go
configHome := filepath.Join(t.TempDir(), "config")
t.Setenv("XDG_CONFIG_HOME", configHome)
```

Add `TestLoadBuildInputsInitializesAndReadsPublicCA` using `proxy.DefaultOptions()` to locate the isolated files. Call `loadBuildInputs(cfg, openBuildScriptsDirectory)`, PEM-decode `inputs.proxyCA`, parse it with `x509.ParseCertificate`, and assert:

```go
if block == nil || block.Type != "CERTIFICATE" {
    t.Fatalf("proxy CA input is not a PEM certificate: %q", inputs.proxyCA)
}
if strings.Contains(string(inputs.proxyCA), "PRIVATE KEY") {
    t.Fatal("proxy CA build input contains private key material")
}
if _, err := os.Stat(options.CAKeyPath); err != nil {
    t.Fatalf("proxy CA key was not initialized: %v", err)
}
```

Add `TestCreateReturnsProxyCAInitializationErrorBeforeConnecting`: replace `XDG_CONFIG_HOME` with a regular file path, call `create`, assert the error contains `initialize proxy CA`, and assert the connector was never called.

Update `TestCreateRunsImageWorkflowInOrder` to expect:

```text
push /root/assets/kanedias-proxy.crt
```

immediately after `push /root/assets/pi-models.json`. Assert the uploaded file is mode `0644`, contains a PEM certificate, does not contain `PRIVATE KEY`, and no uploaded path contains `ca.key`.

- [ ] **Step 2: Run focused tests and verify RED**

Run:

```bash
go test ./internal/image -run 'TestLoadBuildInputsInitializesAndReadsPublicCA|TestCreateReturnsProxyCAInitializationErrorBeforeConnecting|TestCreateRunsImageWorkflowInOrder' -count=1
```

Expected: FAIL because `buildInputs.proxyCA`, proxy CA initialization, and the certificate upload do not exist.

- [ ] **Step 3: Implement minimal public-CA input loading**

Import `github.com/sklarsa/kanedias/internal/proxy`, add `proxyCA []byte` to `buildInputs`, and add:

```go
func loadProxyCA() ([]byte, error) {
    options, err := proxy.DefaultOptions()
    if err != nil {
        return nil, fmt.Errorf("resolve proxy CA paths: %w", err)
    }
    if err := proxy.InitCA(options.CACertPath, options.CAKeyPath); err != nil {
        return nil, fmt.Errorf("initialize proxy CA: %w", err)
    }
    certificate, err := os.ReadFile(options.CACertPath)
    if err != nil {
        return nil, fmt.Errorf("read proxy CA certificate %q: %w", options.CACertPath, err)
    }
    return certificate, nil
}
```

Call `loadProxyCA` from `loadBuildInputs` before it returns and assign the result to `inputs.proxyCA`. Add this entry to the image upload table:

```go
{path: "/root/assets/kanedias-proxy.crt", content: inputs.proxyCA, mode: 0o644},
```

Do not read the private key or add it to `buildInputs`.

- [ ] **Step 4: Run focused and package tests and verify GREEN**

Run:

```bash
go test ./internal/image -run 'TestLoadBuildInputsInitializesAndReadsPublicCA|TestCreateReturnsProxyCAInitializationErrorBeforeConnecting|TestCreateRunsImageWorkflowInOrder' -count=1
go test ./internal/image -count=1
```

Expected: PASS.

- [ ] **Step 5: Review and commit Task 1**

Run `git diff --check`, inspect `git diff -- internal/image`, then commit:

```bash
git add internal/image/image.go internal/image/image_test.go
git commit -m "feat(image): include proxy CA build input"
```

---

### Task 2: Install the CA into Debian Trust and Remove the Runtime Mount

**Files:**
- Modify: `internal/image/install.sh`
- Test: `internal/image/image_test.go`
- Modify: `internal/profiles/sandbox.yaml`
- Modify: `internal/profiles/profiles.go`
- Test: `internal/profiles/profiles_test.go`
- Modify: `docs/architecture/session-supervisor.md`

**Interfaces:**
- Consumes: `/root/assets/kanedias-proxy.crt` produced by Task 1.
- Produces: baked certificate `/usr/local/share/ca-certificates/kanedias-proxy.crt`, regenerated `/etc/ssl/certs/ca-certificates.crt`, and a sandbox profile with CA environment variables but no host disk mount.

- [ ] **Step 1: Write failing installer and profile tests**

Add `TestInstallerInstallsProxyCAIntoSystemTrust` in `internal/image/image_test.go`. Assert `install.sh`:

```go
for _, want := range []string{
    `proxy_ca_file="$assets_dir/kanedias-proxy.crt"`,
    `"$proxy_ca_file"`,
    `/usr/local/share/ca-certificates/kanedias-proxy.crt`,
    `update-ca-certificates`,
} {
    if !strings.Contains(script, want) {
        t.Errorf("installer missing proxy CA trust behavior %q", want)
    }
}
```

Also assert the certificate installation and `update-ca-certificates` occur after the `apt-get install` block so the command is available.

Replace `TestRenderSandboxUsesLifecycleDevicesAndDefaultProxyCA` with `TestRenderSandboxUsesLifecycleDevicesAndBakedProxyCA`. Keep existing security/NIC/workspace assertions, add positive assertions for:

```text
environment.NODE_EXTRA_CA_CERTS: "/usr/local/share/ca-certificates/kanedias-proxy.crt"
environment.SSL_CERT_FILE: "/etc/ssl/certs/ca-certificates.crt"
```

and negative assertions for `proxy-ca:`, `kanedias-proxy/ca.crt` as a `source:`, and any disk device targeting `/usr/local/share/ca-certificates/kanedias-proxy.crt`.

- [ ] **Step 2: Run focused tests and verify RED**

Run:

```bash
go test ./internal/image -run TestInstallerInstallsProxyCAIntoSystemTrust -count=1
go test ./internal/profiles -run TestRenderSandboxUsesLifecycleDevicesAndBakedProxyCA -count=1
```

Expected: FAIL because the installer does not install/update the CA and the profile still mounts it from the host.

- [ ] **Step 3: Implement image trust installation**

In `internal/image/install.sh`, define:

```bash
proxy_ca_file="$assets_dir/kanedias-proxy.crt"
```

Include it in the required-input loop. After `apt-get install` completes, install and register it:

```bash
install -m 0644 "$proxy_ca_file" \
    /usr/local/share/ca-certificates/kanedias-proxy.crt
update-ca-certificates
```

Keep the private key absent from all image inputs and commands.

- [ ] **Step 4: Remove runtime CA path rendering and mount**

Delete the complete `devices.proxy-ca` block from `internal/profiles/sandbox.yaml` while retaining both CA environment variables.

In `internal/profiles/profiles.go`, reduce `templateData` to `ProxyURL string`, remove `ProxyCACertPath`, remove the `os.UserConfigDir`/`filepath.Join` logic, and remove now-unused imports.

Update the proxy prerequisite section in `docs/architecture/session-supervisor.md` to say the public proxy CA is baked into the base image and Debian trust bundle by `kanedias image create`; the sandbox profile supplies proxy and CA environment variables but no longer mounts the host certificate.

- [ ] **Step 5: Run focused, shell, and package tests and verify GREEN**

Run:

```bash
bash -n internal/image/install.sh
go test ./internal/image -run 'TestInstallerInstallsProxyCAIntoSystemTrust|TestCreateRunsImageWorkflowInOrder' -count=1
go test ./internal/profiles -count=1
go test ./internal/image -count=1
```

Expected: PASS.

- [ ] **Step 6: Review and commit Task 2**

Run `git diff --check`, inspect the Task 2 diff, then commit:

```bash
git add internal/image/install.sh internal/image/image_test.go \
  internal/profiles/sandbox.yaml internal/profiles/profiles.go \
  internal/profiles/profiles_test.go docs/architecture/session-supervisor.md
git commit -m "fix(image): trust baked proxy CA"
```

---

### Task 3: Verify Hermetic Suite and Cook the Real Sandbox Image

**Files:**
- No source changes expected.
- Runtime logs and scripts must stay under a mode-private temporary directory.

**Interfaces:**
- Consumes: Task 1 and Task 2 commits plus the configured Incus image alias `sandbox`.
- Produces: a newly published sandbox image that trusts the persistent host proxy CA.

- [ ] **Step 1: Run fresh static and hermetic verification**

Run:

```bash
gofmt -l .
bash -n internal/image/install.sh internal/image/kanedias-pi-env internal/image/kanedias-pi-rpc
git diff --check
make test
```

Expected: no formatting output, shell syntax exit 0, no diff errors, and all Go/Node tests PASS.

- [ ] **Step 2: Build the exact branch binary**

Run:

```bash
run_dir=$(mktemp -d /tmp/kanedias-proxy-ca-e2e.XXXXXX)
chmod 700 "$run_dir"
go build -o "$run_dir/kanedias" .
```

Record the current `sandbox` alias fingerprint and exact Incus instance/custom-volume baseline before mutation.

- [ ] **Step 3: Rebuild and publish the real image**

Run the exact branch binary with the worktree's absolute configuration path:

```bash
"$run_dir/kanedias" --config "$PWD/config.toml" image create \
  >"$run_dir/image-create.log" 2>&1
```

Expected: exit 0 and a new `sandbox` alias fingerprint. If it fails, preserve the log and verify the old alias remains usable.

- [ ] **Step 4: Start an owned proxy and disposable supervised session**

Start:

```bash
"$run_dir/kanedias" --config "$PWD/config.toml" proxy run --request-log \
  >"$run_dir/proxy.log" 2>&1 &
```

Wait by condition until `10.76.111.1:3128` accepts TCP. Then start:

```bash
"$run_dir/kanedias" --config "$PWD/config.toml" session \
  --socket "$run_dir/root.sock" >"$run_dir/session.log" 2>&1 &
```

Poll `/v1/tree` over the Unix socket until lifecycle is `ready`. Record the exact session ID and map it to its Incus instance using `user.kanedias.session_id` metadata. Install a cleanup trap that deletes only this session through its supervisor socket, then stops only the owned proxy.

- [ ] **Step 5: Verify CA identity, bundle trust, and Pi process environment**

Inside the disposable instance, assert:

```bash
sha256sum /usr/local/share/ca-certificates/kanedias-proxy.crt
openssl verify -CAfile /etc/ssl/certs/ca-certificates.crt \
  /usr/local/share/ca-certificates/kanedias-proxy.crt
```

The certificate hash must match the host `~/.config/kanedias-proxy/ca.crt`, and `openssl verify` must return `OK`.

Read the actual running `kanedias-pi@*.service` `MainPID` environment and confirm uppercase/lowercase HTTP(S) proxy variables, `GH_TOKEN`, `SSL_CERT_FILE`, `NODE_EXTRA_CA_CERTS`, and Kanedias session identity are present. Redact `GH_TOKEN`.

- [ ] **Step 6: Verify live `curl`, `gh`, and HTTPS Git behavior**

As UID/GID 1000 with `HOME=/home/kanedias`, run each check with a timeout and capture exit codes:

```bash
curl -fsS -o /dev/null -w 'http=%{http_code} proxy=%{proxy_used}' https://example.com/
curl -fsS -o /dev/null -w 'http=%{http_code} proxy=%{proxy_used}' https://api.github.com/
gh api user --jq .login
gh api graphql -f 'query=query { viewer { login } }' --jq .data.viewer.login
gh repo view sklarsa/kanedias-testing --json nameWithOwner --jq .nameWithOwner
git ls-remote --exit-code https://github.com/sklarsa/kanedias.git HEAD
git ls-remote --exit-code https://github.com/sklarsa/kanedias-testing.git HEAD
git clone --depth=1 https://github.com/sklarsa/kanedias-testing.git "$guest_tmp/git-clone"
git -C "$guest_tmp/git-clone" fetch --force --prune origin
gh repo clone sklarsa/kanedias-testing "$guest_tmp/gh-clone" -- --depth=1
git -C "$guest_tmp/git-clone" push --dry-run origin "HEAD:refs/heads/$unique_branch"
```

Then require `git ls-remote --exit-code origin "refs/heads/$unique_branch"` to exit `2`, proving the dry-run created no remote ref.

- [ ] **Step 7: Tear down and verify exact baseline**

DELETE the root session through `/v1/sessions/{sessionId}`, wait for its process and exact Incus instance/volume to disappear, stop the owned proxy, and compare instance/custom-volume names with the recorded baseline. Preserve unrelated resources and leave the newly published image alias in place.

---

### Task 4: Independent Review, PR, CI, and Merge

**Files:**
- No additional source changes unless review finds a defect.

**Interfaces:**
- Consumes: complete branch diff and Task 3 evidence.
- Produces: reviewed and merged GitHub pull request.

- [ ] **Step 1: Run parallel independent review**

Dispatch fresh reviewers with distinct scopes: correctness/security of CA/key handling; test quality/regression coverage; and live-operability/profile/image lifecycle. Every Critical or Important finding must be fixed by one writer and re-reviewed.

- [ ] **Step 2: Run final verification after review fixes**

Re-run:

```bash
gofmt -l .
bash -n internal/image/install.sh internal/image/kanedias-pi-env internal/image/kanedias-pi-rpc
git diff --check
make test
```

If a review fix changes image inputs, installer behavior, or the profile, repeat the affected live acceptance checks from Task 3.

- [ ] **Step 3: Push and create the PR**

Run:

```bash
git push -u origin fix/proxy-ca-image-trust
gh pr create --base main --head fix/proxy-ca-image-trust \
  --title "fix(image): trust proxy CA in sandbox sessions" \
  --body-file "$reviewed_pr_body"
```

The PR body must summarize the root cause, chosen bake-time architecture, hermetic tests, real image cook, live `curl`/`gh`/Git evidence, and CA-rotation requirement.

- [ ] **Step 4: Wait for required checks and inspect the GitHub diff**

Run `gh pr checks --watch`, then inspect `gh pr diff` and verify the PR contains no commits or files outside this feature.

- [ ] **Step 5: Merge and verify repository state**

Merge only after checks pass and review is clean:

```bash
gh pr merge --merge --delete-branch
```

Verify the PR state is `MERGED`, record the merge commit, fetch `origin/main`, and confirm the merge commit is reachable from `origin/main`.
