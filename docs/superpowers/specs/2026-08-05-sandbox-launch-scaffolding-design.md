# Sandbox Launch Scaffolding Design

## Goal

Provide a machine-local, single-sandbox path for launching a Kanedias image with the persistent workspace and credential-injecting HTTPS proxy configured end to end.

## Profile layout

- `profiles/image-build.yaml` contains only the unprivileged nested-container settings needed while building and testing images.
- `profiles/sandbox.yaml` is the runtime profile. It configures uppercase and lowercase HTTP proxy variables for `http://10.75.177.1:3128`, sets `GH_TOKEN=container-dummy` so GitHub CLI sends requests for proxy-side credential replacement, trusts the proxy CA at `/usr/local/share/ca-certificates/kanedias-proxy.crt`, and defines the inherited `/workspace` device using `default/agent-workspace-seed` as its template source.
- `profiles/lemonade.yaml` is the renamed optional Lemonade GPU service profile.
- Machine-specific values are intentionally hardcoded for this initial scaffolding: host bridge address `10.75.177.1` and host CA path `/home/steven/.config/kanedias-proxy/ca.crt`.

## CA initialization

The proxy gains an `-init-ca` mode. It uses the existing CA loading and generation logic, creating the configured certificate and key only when both are absent, then exits without starting the HTTP server. Existing certificate/key pairs are preserved, while mismatched or unreadable pairs remain errors.

## Sandbox launcher

`launch-sandbox.sh <image> [instance-name]` defaults the instance name to `sandbox` and performs these steps:

1. Initialize the proxy CA through `go run ./proxy -init-ca`.
2. Create the Incus `sandbox` profile when absent and replace its configuration from `profiles/sandbox.yaml`.
3. Copy `default/agent-workspace-seed` to `default/agent-workspace-<instance>` with `--volume-only`. An existing target is an error and is never reused or deleted.
4. Initialize the requested image with the Incus `default` and `sandbox` profiles, recording ownership once initialization succeeds.
5. Override the inherited `workspace` device to use the new per-instance volume before startup.
6. Start the new instance, deleting it if startup fails while leaving pre-existing name collisions untouched.
7. Wait for systemd to finish booting so its tmpfiles setup cannot race certificate installation.
8. Run `update-ca-certificates` inside the new instance.
9. On failure, delete the owned instance first and then the owned workspace clone. On success, leave both resources in place.

The launcher does not own the long-running proxy process. For this machine the proxy runs separately with `go run ./proxy -listen 10.75.177.1:3128`.

## Sandbox removal

`remove_sandbox.sh <instance-name>` derives the owned volume as `default/agent-workspace-<instance-name>`. It determines existence only from successfully returned Incus instance and custom-volume lists, so operational lookup failures stop removal rather than being interpreted as absence. Before deleting an existing instance, it verifies that the instance has a local `workspace` device whose source is that exact volume; a mismatched or missing device is a safety error. It deletes the instance before the volume and stops if instance deletion fails. If the instance is already absent, it removes an orphaned matching volume. If both resources are absent, it succeeds without changes.

Launch and removal acquire the same non-blocking per-instance `flock`, serializing lifecycle operations performed through these scripts. The removal script never derives or deletes `agent-workspace-seed`, and Incus remains responsible for refusing volume deletion when another resource still uses the volume. Direct concurrent Incus lifecycle commands outside these scripts remain unsupported.

## Existing script updates

`build-image.sh` and `test-install.sh` use `profiles/image-build.yaml`. References to the old profile filenames are removed. Existing workspace synchronization behavior is unchanged.

## Verification

Automated checks cover proxy CA initialization, profile contents and naming, per-instance volume naming and device override ordering, launcher default/custom instance names, systemd readiness and retry ordering, collision safety, instance/volume cleanup after partial failures, removal ordering, mismatch refusal, orphan cleanup, and idempotency. Live Incus checks verify clone isolation and full launch/remove behavior. Existing Go and shell tests remain part of the verification run.

## Deferred scope

Supervising the long-running proxy, portable host discovery, bulk sandbox lifecycle management, and removing machine-specific paths are intentionally deferred.
