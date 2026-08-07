# Native Nested Incus Workspace PoC Design

## Summary

Kanedias will maintain a cold, native-Btrfs Incus state volume and create a private copy-on-write clone for each manually managed sandbox. Each sandbox will run its own nested `incusd`, backed by its own cloned `/var/lib/incus`. Independent daemons will never share a writable pool.

This proof of concept is deliberately below the session-supervisor layer. It will modify manual sandbox creation and destruction, but it will not modify session code or the session-supervisor architecture.

## Goals

- Add `kanedias workspace repos sync` for the existing repository synchronization operation.
- Add `kanedias workspace incus sync` to create or update a cold nested-Incus seed volume.
- Use the host's existing Btrfs pool and Incus's recursive same-pool volume copy to preserve copy-on-write sharing, including nested Btrfs subvolumes.
- Mount each private Incus-state clone at `/var/lib/incus` in its sandbox.
- Make manual `sandbox create` and `sandbox destroy` manage the additional volume safely.
- Prove that two sandboxes cloned from one seed can run independent nested Incus daemons concurrently.

## Non-goals

- Do not modify `internal/session/**`, `cmd/session.go`, or `docs/architecture/session-supervisor.md`.
- Do not integrate the seed with the in-progress session supervisor.
- Do not add cron scheduling, immutable generations, retention policies, or automatic base-image rebuilds.
- Do not add disk-pressure monitoring or strict per-sandbox disk quotas.
- Do not support non-Btrfs outer storage pools in this PoC.
- Do not share one writable Incus pool between multiple daemons.
- Do not preserve the unreleased `kanedias workspace sync` command as a compatibility alias.

## User Interface

The workspace command is divided into two explicit resource types:

```console
kanedias workspace repos sync
kanedias workspace incus sync
```

Repository storage keeps its existing layout: the custom volume is mounted at `/workspace`, and repositories are stored below `/workspace/repos`.

Nested Incus state uses a separate custom volume mounted directly at `/var/lib/incus`. There is no symlink, bind mount, or alternate `INCUS_DIR`.

## Configuration

The existing repository configuration remains at `[workspace]` so session code does not need to change. Nested Incus configuration is added beneath it:

```toml
[workspace]
pool = "default"
volume = "kanedias-workspace-seed"
repos = [
    "sklarsa/kanedias",
]

[workspace.incus]
volume = "kanedias-incus-seed"
images = [
    "images:debian/13",
]
```

`workspace.pool` is used for both repository and Incus-state volumes. Keeping them in the same outer pool is required for the Btrfs snapshot-backed copy path.

If `workspace.incus.volume` is omitted, it defaults to `kanedias-incus-seed`. An empty image list is valid and creates an initialized seed without preloaded images; the checked-in configuration will preload `images:debian/13` for the PoC.

## Architecture

### Command organization

`cmd/workspace.go` will expose `repos` and `incus` subcommands, each with a `sync` action. The existing repository synchronization service remains the implementation of `workspace repos sync`. A new service calls the nested-Incus synchronization package for `workspace incus sync`.

The nested-Incus implementation will live outside `internal/session`, under the workspace/storage layer. It will expose operations needed by both the command and manual sandbox lifecycle without taking ownership of an entire sandbox.

### Seed volume synchronization

`workspace incus sync` performs the following sequence:

1. Connect to the outer Incus daemon and resolve `workspace.pool`.
2. Require the outer pool's driver to be `btrfs`; fail before creating resources otherwise.
3. Acquire an exclusive seed lock so no sandbox can clone the seed while it is being mutated.
4. Create `workspace.incus.volume` if it does not exist.
5. Create a uniquely named maintenance container from the configured Kanedias base image.
6. Attach the seed volume to the maintenance container at `/var/lib/incus`.
7. Start the maintenance container and wait for systemd.
8. If the nested daemon is uninitialized, run `incus admin init --minimal` inside the maintenance container.
9. Query the nested default storage pool and require:
   - driver `btrfs`;
   - a native subvolume source under `/var/lib/incus/storage-pools`;
   - no loop image source such as `/var/lib/incus/disks/*.img`.
10. Copy or refresh each configured image into the nested daemon's local image store.
11. Stop both the nested `incus.socket` and `incus.service`, ensuring socket activation cannot restart the daemon.
12. Stop the maintenance container, remove its seed-volume device, and delete the container while preserving the seed volume.

The seed must only be cloned while cold. Sandbox creation takes a shared seed lock around the copy operation; synchronization takes the exclusive form for its full mutation and quiescing sequence. Multiple cold clone operations may proceed concurrently.

If synchronization creates a new seed and then fails, cleanup removes the failed maintenance container and the newly created seed. If an existing seed fails during refresh, cleanup always quiesces and removes the maintenance container but preserves the seed for diagnosis. Atomic replacement of an existing seed is deferred with versioned generations.

### Native nested Btrfs mechanics

The outer custom volume is a Btrfs subvolume. Initializing nested Incus natively creates further Btrfs subvolumes beneath `/var/lib/incus/storage-pools/default` for images, instances, and snapshots.

For a same-pool custom-volume copy, Incus's Btrfs driver recursively snapshots the root volume and each nested subvolume. Therefore a sandbox receives an independent tree of writable subvolumes whose unchanged extents are shared with the seed. There is no fixed-size loop file and no shared writable daemon state.

The PoC intentionally accepts the documented nested-Btrfs quota limitation: newly created child subvolumes do not automatically inherit the outer parent qgroup. Host-wide disk-pressure protection will be designed separately.

### Sandbox creation

Manual `sandbox create` retains its current root and repository-volume behavior and adds:

1. Verify that the configured Incus-state seed exists.
2. Under a shared seed lock, copy it to `kanedias-incus-<sandbox-name>`.
3. Add an `incus-state` disk device with:

   ```yaml
   type: disk
   pool: <workspace.pool>
   source: kanedias-incus-<sandbox-name>
   path: /var/lib/incus
   ```

4. Start the sandbox and wait for its outer systemd as today.
5. Wait for the nested daemon to become ready and verify that its default pool still reports the `btrfs` driver.

Failure rollback tracks the Incus-state clone independently. After stopping and deleting a partially created sandbox, cleanup deletes both cloned custom volumes. The protected seed is never selected by sandbox-derived naming.

### Sandbox destruction

Before destructive operations, `sandbox destroy` verifies both expected local devices:

- the repository workspace source is `kanedias-workspace-<sandbox-name>`;
- the Incus-state source is `kanedias-incus-<sandbox-name>`.

If an existing instance has a missing or unexpected source, destruction refuses to proceed rather than deleting an unrelated custom volume. Once verified, destruction stops and deletes the instance, then deletes both deterministic clone volumes. An absent instance does not prevent cleanup of correctly named orphan volumes.

### Sandbox profile

The existing unprivileged sandbox profile keeps `security.nesting=true` and adds:

```yaml
security.syscalls.intercept.mknod: "true"
security.syscalls.intercept.setxattr: "true"
```

The PoC does not enable `security.privileged` on the outer sandbox.

### Base image

The current base image already installs `incus-base`, adds the managed user to `incus-admin`, and deliberately does not initialize the daemon. That remains the correct division:

- the outer image supplies binaries and systemd units;
- `workspace incus sync` initializes and populates mutable daemon state on the seed volume.

## Concurrency and Consistency

- One nested daemon exclusively owns each sandbox's cloned state volume.
- The seed daemon only runs in the maintenance container during synchronization.
- The seed is quiesced before the maintenance container is deleted.
- An exclusive/shared file lock prevents a synchronization-versus-clone race across Kanedias processes on the host.
- Outer Incus remains responsible for serializing individual storage operations.
- The fixed seed is mutable in this PoC. Existing clones are independent and remain valid after later seed changes, but crash-safe generation switching is deferred.

## Error Handling

- Reject a non-Btrfs outer pool before creating a seed or maintenance container.
- Reject successful initialization that selected `dir` or a loop-backed Btrfs pool.
- Treat inability to stop the nested daemon as a synchronization failure; never knowingly publish a hot seed.
- Always attempt maintenance-container cleanup with a bounded context independent of caller cancellation.
- Track submitted outer Incus operations so ambiguous failures still trigger safe cleanup, following existing lifecycle patterns.
- On sandbox creation failure, delete only resources known to have been created by that invocation.
- On sandbox destruction, verify device ownership before deleting deterministic volumes.
- Join cleanup errors with the primary error so leaked-resource risks remain visible.

## Testing

### Unit tests

- CLI routing tests for `workspace repos sync` and `workspace incus sync`, including the absence of the old direct `workspace sync` action.
- Configuration tests for nested volume defaults and configured image references.
- Profile rendering tests for the two required syscall interception settings while retaining an unprivileged outer sandbox.
- Seed synchronization tests using a recording client, covering:
  - outer driver rejection;
  - new versus existing seed behavior;
  - native inner-Btrfs validation;
  - rejection of `dir` and loop-backed sources;
  - image refresh ordering;
  - cold shutdown before maintenance-container deletion;
  - rollback and cleanup errors.
- Manual sandbox tests covering:
  - cloning and attaching both volumes;
  - nested daemon readiness;
  - rollback after each significant failure point;
  - protected seed handling;
  - device ownership checks during destruction;
  - orphan clone cleanup.

### Opt-in live test

A build-tagged, environment-gated live test will:

1. Require a Btrfs outer pool and the configured Kanedias base image.
2. Build a uniquely named Incus-state seed containing one small Linux image.
3. Make two recursive same-pool copies of the cold seed.
4. Launch two outer sandboxes concurrently with their private copies mounted at `/var/lib/incus`.
5. Verify that both nested daemons report a native `btrfs` default pool and can see the cached image.
6. Launch one inner container per sandbox.
7. Write distinct marker data and verify that neither nested daemon can see the other's instance or marker.
8. Stop and delete all inner and outer instances and all temporary custom volumes.

The live test proves functional recursive cloning and isolation. Exact physical extent accounting and disk-pressure behavior are outside this PoC.

## Integration with the Session Supervisor

This work deliberately provides a lower-level storage primitive. The future supervisor can clone the cold seed, add the returned disk device to its own instance request, and register the clone in its cleanup state. No current session code or supervisor architecture is changed.

The likely future integration surface is limited to:

- configuration access;
- clone naming and creation;
- one instance disk device;
- one cleanup callback.

## Deferred Work

- Immutable paired outer-image and Incus-state generations.
- Scheduled rebuild and atomic activation.
- Old-generation garbage collection.
- Host free-space watermarks and admission control.
- Strict quota alternatives.
- Server-certificate regeneration for remotely exposed nested daemons.
- Compatibility across development branches with older Incus database schemas.
