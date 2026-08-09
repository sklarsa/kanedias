# Configurable Image Build Scripts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Separate the minimal Kanedias image runtime from operator-specific tools, run configured image-build scripts deterministically, verify the result, rebuild the Incus image, and merge the change through a pull request.

**Architecture:** `config.Config` resolves an optional `base_image.build_scripts_dir` relative to the loaded TOML file. The image package preflights executable direct-child shell scripts into `buildInputs`, runs the embedded core installer, then uploads and directly executes custom scripts as root before stopping and publishing the image. Numbered repository scripts retain the current optional tools while Claude Code and the custom Pi theme are removed.

**Tech Stack:** Go 1.x, `github.com/pelletier/go-toml/v2`, Incus v7 client APIs, Bash, shellcheck, Cobra CLI, GitHub CLI.

## Global Constraints

- `base_image.build_scripts_dir` is optional; an omitted value runs no custom scripts.
- Relative script-directory paths resolve from the directory containing `config.toml`; absolute paths remain absolute.
- Discovery is non-recursive and considers only direct children ending in `.sh`.
- Only regular `.sh` files with at least one execute bit run; non-executable regular `.sh` files and non-`.sh` entries are ignored.
- Executable `.sh` symlinks and other executable non-regular `.sh` entries are errors.
- Selected scripts run directly as root in lexical filename order after the core installer and Pi-extension verification.
- Custom script stdout/stderr streams through the image command's existing writers.
- Any custom-script failure prevents publication and preserves bounded detached cleanup.
- The embedded core retains Kanedias machinery and small general utilities but excludes cloud tools, container tools, GCC/Clang, Go, Pulumi, uv, tfenv, Superpowers, `pi-web-suite`, OpenAI Fast, and personal tmux settings.
- Claude Code and the custom Pi theme are removed entirely.
- The final workflow must rebuild the configured Incus image, create a pull request, and squash-merge it to `main` only after review and verification pass.

## File Structure

- Modify `internal/config/config.go`: decode and resolve the optional build-script directory.
- Modify `internal/config/config_test.go`: prove TOML decoding and path semantics.
- Modify `internal/image/image.go`: preflight, upload, and execute custom scripts; remove theme/tmux core inputs.
- Modify `internal/image/image_test.go`: prove discovery, ordering, workflow, streaming, errors, cleanup, and removed inputs.
- Modify `internal/image/install.sh`: retain only core runtime and small utility installation.
- Create `image-build.d/10-cloud-tools.sh`: cloud CLIs and AWS Session Manager plugin.
- Create `image-build.d/20-container-tools.sh`: Docker/Podman and Kubernetes helpers.
- Create `image-build.d/30-dev-toolchains.sh`: compilers, Go, Pulumi, uv, and tfenv.
- Create `image-build.d/40-pi-extras.sh`: Superpowers, `pi-web-suite`, and OpenAI Fast.
- Create `image-build.d/50-user-config.sh`: personal tmux settings.
- Modify `assets/pi-settings.json`: remove theme and custom package registrations from core settings.
- Delete `assets/cobalt-ember.json`: removed theme.
- Delete `assets/tmux.conf`: personal config now comes from a custom script.
- Modify `config.toml`: select `image-build.d` for this repository's image.

## Execution Topology

After creating the feature branch, Tasks 1 and 2 may run concurrently in isolated worktrees because they modify disjoint files. Integrate and review both commits before Task 3, which consumes Task 1's config interface and Task 2's script layout. Task 4 is the integration/verification lane. Run independent spec and quality reviews after implementation, apply fixes through a single writer, then rebuild, open the PR, and merge.

---

### Task 1: Decode and Resolve the Build-Script Directory

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

**Interfaces:**
- Consumes: `Config.Dir`, which `Load` already sets to the absolute config directory.
- Produces: `BaseImage.BuildScriptsDir string` with TOML key `build_scripts_dir`.
- Produces: `func (cfg Config) BuildScriptsPath() string`, returning `""` when omitted, a cleaned absolute value unchanged in meaning, or `filepath.Join(cfg.Dir, cfg.BaseImage.BuildScriptsDir)` for relative values.

- [ ] **Step 1: Write failing decoding and path tests**

Extend `TestLoadLifecycleConfig` with:

```go
build_scripts_dir = "image-build.d"
```

and expect:

```go
BuildScriptsDir: "image-build.d",
```

Add:

```go
func TestBuildScriptsPath(t *testing.T) {
	dir := t.TempDir()
	absolute := filepath.Join(t.TempDir(), "scripts")
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{name: "omitted", cfg: Config{Dir: dir}, want: ""},
		{name: "relative", cfg: Config{Dir: dir, BaseImage: BaseImage{BuildScriptsDir: "image-build.d"}}, want: filepath.Join(dir, "image-build.d")},
		{name: "absolute", cfg: Config{Dir: dir, BaseImage: BaseImage{BuildScriptsDir: absolute}}, want: absolute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.BuildScriptsPath(); got != tt.want {
				t.Fatalf("BuildScriptsPath() = %q, want %q", got, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the focused tests and confirm RED**

Run:

```bash
go test ./internal/config -run 'TestLoadLifecycleConfig|TestBuildScriptsPath' -count=1
```

Expected: compilation fails because `BuildScriptsDir` and `BuildScriptsPath` do not exist.

- [ ] **Step 3: Add the config field and resolver**

Update `BaseImage`:

```go
type BaseImage struct {
	Name            string   `toml:"name"`
	Source          string   `toml:"source"`
	Image           string   `toml:"image"`
	AuthorizedHosts []string `toml:"authorized_hosts"`
	BuildScriptsDir string   `toml:"build_scripts_dir"`
}
```

Add:

```go
func (cfg Config) BuildScriptsPath() string {
	if cfg.BaseImage.BuildScriptsDir == "" {
		return ""
	}
	if filepath.IsAbs(cfg.BaseImage.BuildScriptsDir) {
		return filepath.Clean(cfg.BaseImage.BuildScriptsDir)
	}
	return filepath.Join(cfg.Dir, cfg.BaseImage.BuildScriptsDir)
}
```

- [ ] **Step 4: Run config tests and confirm GREEN**

Run:

```bash
gofmt -w internal/config/config.go internal/config/config_test.go
go test ./internal/config -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit the config interface**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): resolve image build script directory"
```

---

### Task 2: Extract Operator Additions and Minimize the Core Installer

**Files:**
- Modify: `internal/image/install.sh`
- Modify: `internal/image/image_test.go`
- Modify: `assets/pi-settings.json`
- Create: `image-build.d/10-cloud-tools.sh`
- Create: `image-build.d/20-container-tools.sh`
- Create: `image-build.d/30-dev-toolchains.sh`
- Create: `image-build.d/40-pi-extras.sh`
- Create: `image-build.d/50-user-config.sh`

**Interfaces:**
- Consumes: Debian 13 image, root execution, and `/home/kanedias` conventions.
- Produces: an embedded installer that establishes the core preconditions listed under Global Constraints.
- Produces: self-contained executable Bash scripts whose lexical order is their execution order.

- [ ] **Step 1: Change installer-boundary tests to express the new contract**

Replace `TestInstallerEnablesOpenAIFastByDefault` with a test that reads `../../image-build.d/40-pi-extras.sh` and asserts:

```go
for _, forbidden := range []string{
	"install_cloud_apt_packages",
	"install_aws_cli",
	"install_session_manager_plugin",
	"install_container_tools",
	"install_claude_code",
	"install_go",
	"install_pulumi",
	"install_uv",
	"install_tfenv",
	"pi-web-suite",
	"superpowers",
	"openai-fast.json",
	"cobalt-ember",
} {
	if strings.Contains(string(installer), forbidden) {
		t.Errorf("core installer contains operator addition %q", forbidden)
	}
}
```

Assert the Pi extras script contains all of:

```go
[]string{
	"git:github.com/obra/superpowers",
	"npm:pi-web-suite",
	"npm:@diegopetrucci/pi-openai-fast",
	"openai-fast.json",
}
```

Add a table that reads every planned numbered script and checks the expected tool markers. Check with `os.Stat` that each script has an execute bit. Read `assets/pi-settings.json`, decode it into `map[string]any`, and assert that `theme` and `packages` are absent.

- [ ] **Step 2: Run focused image tests and confirm RED**

Run:

```bash
go test ./internal/image -run 'TestInstaller|TestCustomBuildScripts|TestCorePiSettings' -count=1
```

Expected: FAIL because the embedded installer still contains operator additions and the numbered scripts do not exist.

- [ ] **Step 3: Reduce `internal/image/install.sh` to core behavior**

Keep root/Debian validation, core asset checks, the initial lightweight apt install, user creation, NVM/Node/Pi, embedded extension, runtime directories, and RPC systemd units.

Make these exact boundary edits:

- Remove `clang`, `gcc`, and the redundant distro `nodejs` from the initial apt list.
- Remove tmux/theme asset variables, required-file checks, and installation commands.
- Remove `pi install` calls for Superpowers and `pi-web-suite`.
- Remove creation of `extensions/openai-fast.json`.
- Delete the functions and bottom-level calls for cloud apt packages, AWS CLI, Session Manager, container tools, Claude Code, Go, Pulumi, uv, and tfenv.
- Leave `install_nvm`, `install_pi`, `install_pi_extension`, and `install_pi_rpc_service` in the final call sequence.

- [ ] **Step 4: Create self-contained numbered scripts**

Copy the current installation bodies without changing download verification or architecture checks, grouped as follows:

```text
10-cloud-tools.sh       install_cloud_apt_packages, install_aws_cli, install_session_manager_plugin
20-container-tools.sh   install_container_tools
30-dev-toolchains.sh    apt install clang gcc, install_go, install_pulumi, install_uv, install_tfenv
40-pi-extras.sh         managed-user pi install for all three packages, then owned openai-fast.json
50-user-config.sh       write the existing two tmux settings as /home/kanedias/.tmux.conf
```

Every script starts with:

```bash
#!/usr/bin/env bash
set -Eeuo pipefail

if (( EUID != 0 )); then
    echo "$(basename "$0") must run as root" >&2
    exit 1
fi

export DEBIAN_FRONTEND=noninteractive
```

Scripts that act as the managed user define `managed_user=kanedias`, `managed_home=/home/kanedias`, and invoke `runuser` with explicit `HOME`, `USER`, and `LOGNAME`. `40-pi-extras.sh` sources `$managed_home/.nvm/nvm.sh` inside that managed-user shell before invoking `pi install`.

Mark all five files executable:

```bash
chmod 0755 image-build.d/*.sh
```

- [ ] **Step 5: Remove operator registrations from core Pi settings**

Change `assets/pi-settings.json` to retain the changelog marker, provider/model/thinking defaults, and `hideThinkingBlock`, while deleting both:

```json
"theme": "cobalt-ember"
```

and the complete `packages` array.

- [ ] **Step 6: Run shell and focused Go checks**

Run:

```bash
bash -n internal/image/install.sh image-build.d/*.sh
shellcheck internal/image/install.sh image-build.d/*.sh
go test ./internal/image -run 'TestInstaller|TestCustomBuildScripts|TestCorePiSettings' -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit the installation split**

```bash
git add internal/image/install.sh internal/image/image_test.go assets/pi-settings.json image-build.d
git commit -m "refactor(image): split core and operator installs"
```

---

### Task 3: Discover and Execute Configured Build Scripts

**Files:**
- Modify: `internal/image/image.go`
- Modify: `internal/image/image_test.go`
- Delete: `assets/cobalt-ember.json`
- Delete: `assets/tmux.conf`

**Interfaces:**
- Consumes: `func (cfg config.Config) BuildScriptsPath() string` from Task 1.
- Consumes: executable direct-child scripts created in Task 2.
- Produces: `type buildScript struct { name string; content []byte }`.
- Produces: `func loadBuildScripts(cfg config.Config) ([]buildScript, error)`.
- Extends: `buildInputs` with `scripts []buildScript` and removes `piTheme`/`tmuxConfig`.

- [ ] **Step 1: Write failing discovery tests**

Add a helper that creates a config with `BaseImage.BuildScriptsDir` and writes entries under `cfg.BuildScriptsPath()`. Add tests covering:

```go
func TestLoadBuildScriptsFiltersAndSorts(t *testing.T)
func TestLoadBuildScriptsRejectsExecutableSymlink(t *testing.T)
func TestCreateReadsBuildScriptsBeforeConnecting(t *testing.T)
```

The filtering test creates executable `20-second.sh` and `10-first.sh`, non-executable `ignored.sh`, non-shell `ignored.txt`, and a nested executable script. Expect exactly:

```go
[]buildScript{
	{name: "10-first.sh", content: []byte("#!/bin/sh\necho first\n")},
	{name: "20-second.sh", content: []byte("#!/bin/sh\necho second\n")},
}
```

The symlink test creates an executable target outside the configured directory and a `linked.sh` symlink inside it; expect an error containing `linked.sh` and `regular file`.

Make the preflight test table-driven with missing, unreadable (`chmod 0000`, restored with `t.Cleanup`), and regular-file-instead-of-directory paths. Each case asserts `connected == false` and an error containing `read image build scripts`.

- [ ] **Step 2: Extend the workflow test with two scripts and confirm RED**

Configure `10-first.sh` and `20-second.sh` in `TestCreateRunsImageWorkflowInOrder`. After `exec test -d /opt/kanedias/pi-extension/node_modules/typebox`, expect:

```go
"exec install -d -m 0700 /root/build-scripts",
"push /root/build-scripts/10-first.sh",
"push /root/build-scripts/20-second.sh",
"exec /root/build-scripts/10-first.sh",
"exec /root/build-scripts/20-second.sh",
```

Remove the theme/tmux asset upload expectations. Assert uploaded scripts have mode `0700` and exact preflighted contents.

Run:

```bash
go test ./internal/image -run 'TestLoadBuildScripts|TestCreateRunsImageWorkflowInOrder|TestCreateReadsBuildScriptsBeforeConnecting' -count=1
```

Expected: FAIL because script discovery and workflow support are absent.

- [ ] **Step 3: Implement preflight discovery**

Add imports for `path/filepath` and `sort`. Implement `loadBuildScripts` with this flow:

```go
path := cfg.BuildScriptsPath()
if path == "" {
	return nil, nil
}
entries, err := os.ReadDir(path)
// Wrap directory errors as: read image build scripts %q: %w
for each direct entry with strings.HasSuffix(name, ".sh"):
    obtain lstat-style mode from the directory entry
    if no execute bit: continue
    if mode is not regular: return an error naming the entry and requiring a regular file
    read filepath.Join(path, name), wrapping the filename on error
    append buildScript{name: name, content: content}
sort by buildScript.name
return scripts
```

Call this function from `loadBuildInputs` after core assets and the rendered profile are loaded. Any error returns an empty `buildInputs`.

- [ ] **Step 4: Remove theme/tmux core inputs**

Delete `piTheme` and `tmuxConfig` from `buildInputs`, remove `cobalt-ember.json` and `tmux.conf` from `loadBuildInputs`, remove their upload entries, and delete both asset files from the repository.

Update the `imageConfig` test helper so it creates only `pi-settings.json`, `pi-auth.json`, and `pi-models.json`.

- [ ] **Step 5: Implement upload and direct execution**

After extension dependency verification and only when `len(inputs.scripts) > 0`:

```go
if _, _, err := client.Exec(ctx, instanceName, incusclient.ExecRequest{
	Command: []string{"install", "-d", "-m", "0700", "/root/build-scripts"},
}); err != nil {
	return fmt.Errorf("create image build script directory: %w", err)
}
for _, script := range inputs.scripts {
	destination := "/root/build-scripts/" + script.name
	if err := client.PushFile(ctx, instanceName, destination, script.content, 0o700); err != nil {
		return fmt.Errorf("upload image build script %q: %w", script.name, err)
	}
}
for _, script := range inputs.scripts {
	_, _ = fmt.Fprintf(stdout, "Running image build script %s...\n", script.name)
	path := "/root/build-scripts/" + script.name
	if _, _, err := client.Exec(ctx, instanceName, incusclient.ExecRequest{
		Command: []string{path}, Stdout: stdout, Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("run image build script %q: %w", script.name, err)
	}
}
```

Upload all scripts before executing the first one so an upload failure cannot leave a partially executed custom set.

- [ ] **Step 6: Add named failure, streaming, and cleanup coverage**

Extend `recordingClient` with a command-aware execution hook:

```go
execCommand func([]string) error
```

Call it with `request.Command` before returning normal output. Add a test where `20-fail.sh` returns a sentinel error. Assert:

- The returned error names `20-fail.sh` and satisfies `errors.Is(err, sentinel)`.
- Output from `10-first.sh` was streamed.
- No later script, stop-for-publish, or publish call occurred.
- Final calls are forced cleanup stop and cleanup delete.

Add a separate empty/omitted configuration test asserting no `/root/build-scripts` operation occurs. Add an upload-failure case keyed to `20-second.sh`; assert no custom script executes, publication does not occur, the error names the failed upload, and forced cleanup completes.

- [ ] **Step 7: Run focused and package tests**

Run:

```bash
gofmt -w internal/image/image.go internal/image/image_test.go
go test ./internal/config ./internal/image -count=1
```

Expected: PASS.

- [ ] **Step 8: Commit custom-script orchestration**

```bash
git add internal/image/image.go internal/image/image_test.go assets/cobalt-ember.json assets/tmux.conf
git commit -m "feat(image): run configured build scripts"
```

---

### Task 4: Configure, Integrate, and Verify the Repository Image

**Files:**
- Modify: `config.toml`

**Interfaces:**
- Consumes: all prior task outputs.
- Produces: the repository's configured `image-build.d` selection and a fully verified branch.

- [ ] **Step 1: Configure the repository build-script directory**

Add to `[base_image]`:

```toml
build_scripts_dir = "image-build.d"
```

- [ ] **Step 2: Run asset and boundary searches**

Run:

```bash
rg -n 'cobalt-ember|install_claude_code|claude-code' internal/image image-build.d assets config.toml
rg -n 'install_cloud_apt_packages|install_container_tools|install_go|install_pulumi|install_uv|install_tfenv|pi-web-suite|superpowers|openai-fast.json' internal/image/install.sh
```

Expected: the first command returns no matches. The second command returns no matches. Confirm the expected extra-tool markers do appear under `image-build.d`:

```bash
rg -n 'azure-cli|awscli|session-manager|docker-ce|podman|k9s|kind|clang|gcc|go.dev|pulumi|astral.sh/uv|tfenv|pi-web-suite|superpowers|openai-fast' image-build.d
```

Expected: every operator addition is represented in its numbered script.

- [ ] **Step 3: Run shell checks**

```bash
bash -n internal/image/install.sh image-build.d/*.sh
shellcheck internal/image/install.sh image-build.d/*.sh
```

Expected: PASS with no diagnostics.

- [ ] **Step 4: Run formatting and the complete hermetic suite**

```bash
gofmt -w .
git diff --check
go test ./...
node --test internal/server/web/*.test.js
golangci-lint run ./...
```

Expected: all commands PASS.

- [ ] **Step 5: Commit configuration and integration fixes**

```bash
git add config.toml
git add -u
git commit -m "chore(image): configure operator build scripts"
```

If no integration fixes were needed beyond `config.toml`, this commit contains only that file.

---

### Task 5: Independent Review, Image Rebuild, Pull Request, and Merge

**Files:**
- Review: all changes from `origin/main...HEAD`
- Operational output: Incus image alias configured by `base_image.name`

**Interfaces:**
- Consumes: verified feature branch from Task 4.
- Produces: independently reviewed changes, a successfully rebuilt image, a merged GitHub pull request, and updated local `main`.

- [ ] **Step 1: Run parallel independent reviews**

Dispatch fresh read-only reviewers in parallel:

1. Spec reviewer: compare `origin/main...HEAD` to `docs/superpowers/specs/2026-08-09-configurable-image-build-scripts-design.md` and report missing or extra behavior.
2. Code-quality/security reviewer: inspect path handling, symlink behavior, root execution, script ordering, error wrapping, cleanup, shell safety, package verification, and tests.

Do not give reviewers write access. Collect findings with file/line evidence.

- [ ] **Step 2: Apply valid review fixes through one writer**

For each substantiated finding, write a failing regression test, run it to confirm RED, implement the smallest correction, and rerun the focused test to GREEN. Commit fixes as:

```bash
git commit -am "fix(image): address build script review"
```

Stage newly created regression files explicitly if a fix adds any.

- [ ] **Step 3: Re-run final verification from a clean status**

```bash
git status --short
git diff --check origin/main...HEAD
bash -n internal/image/install.sh image-build.d/*.sh
shellcheck internal/image/install.sh image-build.d/*.sh
go test ./...
node --test internal/server/web/*.test.js
golangci-lint run ./...
```

Expected: empty status and all checks PASS.

- [ ] **Step 4: Build the CLI and rebuild/publish the Incus image**

```bash
make build
./bin/kanedias --config config.toml image create
```

Expected: the command streams the core installer and each numbered script in order and ends with `Published image sandbox.`. If an external service, package repository, network, or Incus failure occurs, preserve the logs, report the failure, and do not claim successful publication.

- [ ] **Step 5: Push and create the pull request**

```bash
git push -u origin feat/configurable-image-build-scripts
gh pr create \
  --base main \
  --head feat/configurable-image-build-scripts \
  --title "feat(image): support configurable build scripts" \
  --body-file /tmp/kanedias-configurable-image-build-scripts-pr.md
```

The PR body must summarize the core/custom split, configuration semantics, deliberate Claude/theme removal, tests/reviews, and successful image publication.

- [ ] **Step 6: Check PR status and squash-merge**

```bash
gh pr checks --watch
gh pr merge --squash --delete-branch
```

Expected: required checks pass and GitHub reports the PR merged. Do not bypass failing required checks.

- [ ] **Step 7: Synchronize and verify local main**

```bash
git switch main
git pull --ff-only origin main
git status --short
git log -1 --oneline --decorate
```

Expected: local `main` is clean and points to the merged PR commit.
