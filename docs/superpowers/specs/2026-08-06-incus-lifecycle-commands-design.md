# Incus Lifecycle Commands Design

## Goal

Replace the image, sandbox, and workspace shell workflows with small Cobra commands backed entirely by the Incus Go client:

```text
kanedias image create
kanedias sandbox create [name]
kanedias sandbox destroy [name]
kanedias workspace sync
```

Both sandbox commands default `name` to `sandbox`. The implementation should get the existing workflows working end to end, remain clean, and avoid speculative abstractions or exhaustive edge-case machinery.

## Configuration

The local `config.toml` is ignored by Git. The tracked copy will be removed without deleting the user's local file. Committed files in `assets/` remain tracked and resolve relative to the directory containing the selected config file.

The new configuration model is:

```toml
[base_image]
name = "sandbox"
source = "https://images.linuxcontainers.org"
image = "debian/13"
authorized_hosts = []

[workspace]
pool = ""
volume = "kanedias-workspace-seed"
repos = []
```

`base_image.name`, `base_image.source`, and `base_image.image` are required by lifecycle commands. The source is an explicit SimpleStreams URL rather than an Incus CLI-style named remote. `authorized_hosts` may be empty; image creation still writes an empty `/root/assets/authorized_hosts` file.

`workspace.pool` is optional. When omitted, the command uses the connected server's sole storage pool and fails clearly if there are zero or multiple pools. `workspace.volume` defaults to `kanedias-workspace-seed`. An empty repository list is allowed; `workspace sync` prints a warning and still prepares the seed volume.

The managed user remains the constant `kanedias`. DNS and system-readiness timeouts remain fixed constants of 60 seconds. Cleanup has a fixed 30-second timeout. Sandbox locks use `<system-temp>/kanedias-sandbox-locks-<uid>` and are not configurable in this iteration.

## Incus Integration

All Incus operations use `github.com/lxc/incus` Go APIs. No production path invokes the `incus` executable.

Destination operations connect through the default local Unix socket. Image creation connects to the configured SimpleStreams URL to resolve `base_image.image`, then creates the instance on the local destination.

A small `internal/incusclient` package provides only shared mechanics needed by the workflows:

- local and SimpleStreams connection;
- context-aware operation waiting;
- instance command execution and file upload;
- storage-pool resolution;
- create-or-update profile application.

The package is a thin adapter, not a generic orchestration framework. Image, sandbox, workspace, and network behavior remain in their respective packages.

Every request and long-running operation uses the Cobra command context. Cancellation stops new work and returns a context error. Cleanup of resources already created uses `context.WithoutCancel` plus the fixed 30-second timeout, so Ctrl-C cancels the requested operation without preventing owned temporary resources from being removed. Cleanup errors are joined with the primary failure.

The existing managed `kanedias` network is also migrated from CLI execution to the Go client.

## Profiles and Network

Before launching an instance, each workflow ensures its required profile exists and matches the embedded definition:

- `image create` ensures the `image-build` profile;
- `sandbox create` and `workspace sync` ensure the `sandbox` profile.

Profile updates use the Go client directly. The sandbox profile overrides `eth0` onto the managed `kanedias` bridge, ensuring its configured proxy endpoint is reachable. Sandbox and workspace commands ensure the bridge before applying the profile.

The inherited workspace disk is removed from the sandbox profile. Commands attach the selected volume as a local instance device, avoiding hardcoded pool names in the reusable profile. Proxy environment and CA mount settings remain profile-managed; the CA source uses the proxy's default certificate path.

## Image Creation

`kanedias image create` replaces `build-image.sh` end to end:

1. Validate image configuration and load the three committed asset files relative to the config directory.
2. Connect to the local Incus server and configured SimpleStreams source.
3. Ensure the `image-build` profile.
4. Create a uniquely named temporary container from `base_image.image` with the `default` and `image-build` profiles.
5. Upload the embedded installer to `/root/install.sh`.
6. Generate `/root/assets/authorized_hosts` from configuration and upload the committed Pi settings, theme, and tmux configuration.
7. Execute `/root/install.sh` inside the container.
8. Stop the container and publish it under `base_image.name`, updating that alias when it already exists.
9. Delete the temporary container.

`install.sh` moves to `internal/image/install.sh` and is the only shell script embedded with `go:embed`.

## Sandbox Lifecycle

`kanedias sandbox create [name]` replaces `launch-sandbox.sh`:

1. Load config, resolve the pool, acquire the per-name lifecycle lock, initialize the proxy CA, ensure the managed network, and ensure the sandbox profile.
2. Copy the seed volume to `kanedias-workspace-<name>`.
3. Create the instance from `base_image.name`, applying `default` and `sandbox` profiles plus a local workspace device for the cloned volume.
4. Start it, wait up to 60 seconds for systemd to report running or degraded, and update trusted CA certificates.
5. On failure or cancellation, delete only the instance and volume created by this invocation.

`kanedias sandbox destroy [name]` replaces `remove_sandbox.sh`. It uses the same lock, verifies that an existing instance's local workspace device names the expected `kanedias-workspace-<name>` volume, deletes the instance before the volume, protects the seed volume, and succeeds when both are already absent.

## Workspace Synchronization

`kanedias workspace sync` replaces both halves of `sync-workspace.sh` without copying or invoking that script:

1. Validate repository slugs and duplicate destination names before side effects.
2. Resolve the pool and create or reuse the configured seed volume. If the repository list is empty, print a warning and return successfully at this point.
3. Initialize the proxy CA, ensure the managed network and sandbox profile, then create a uniquely named temporary instance from `base_image.name` with the seed attached as a local workspace device.
4. Start it, wait up to 60 seconds for GitHub DNS, update CA trust, and prepare `/workspace/repos` for user `kanedias`.
5. Use Incus exec operations to configure GitHub authentication and HTTPS URL rewrites.
6. For each repository, clone when absent or force-refresh the existing self-contained checkout to the remote default branch. Preserve the current destructive behavior: prune tags, hard reset, clean untracked files, and force-update recursive submodules.
7. Stop, detach, and delete the temporary instance while preserving the seed volume.

## Commands and Output

Cobra handlers only load configuration, connect dependencies, and call workflow functions. They add no lifecycle flags in this iteration. Progress messages go to command stdout; warnings and cleanup failures go to stderr. Errors remain concise and retain operation context.

The existing `profile`, `proxy`, and completion command behavior remains available.

## Tests and Legacy Removal

Tests use narrow fake interfaces around the Incus adapter. They focus on useful behavior rather than exhaustive permutations:

- config defaults and required lifecycle values;
- profile ensure-before-launch ordering;
- happy-path image, sandbox, and workspace sequencing;
- cancellation propagation and cleanup with a non-cancelled bounded context;
- sandbox ownership checks and delete ordering;
- empty-repository warning and destructive refresh command sequence;
- Cobra hierarchy, defaults, and context propagation;
- confirmation that production code does not execute the Incus CLI.

Live Incus tests remain opt-in behind the `incus` build tag. Shell workflow harnesses are replaced by Go tests where appropriate.

After migration, remove:

- `build-image.sh`;
- `launch-sandbox.sh`;
- `remove_sandbox.sh`;
- `sync-workspace.sh`;
- `test-install.sh`;
- `test-launch-sandbox.sh`;
- `test-remove-sandbox.sh`;
- `test-sync-workspace.sh`.

The root `install.sh` is moved into `internal/image/` rather than deleted. No unrelated installation-script refactor is included.
