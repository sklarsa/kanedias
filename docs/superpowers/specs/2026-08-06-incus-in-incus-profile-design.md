# Incus-in-Incus Profile Design

## Goal

Add a separate additive Incus profile that allows an unprivileged outer system container to run nested Incus containers. Determine the profile settings through a live test on this host rather than assuming settings beyond Incus's documented nesting option.

## Scope

The profile is named `incus-in-incus`. It is intended to be applied alongside a base profile such as `default` and therefore defines no devices. It does not install or configure Incus in the guest, provide root or NIC devices, support nested virtual machines, or modify the existing `image-build` and `sandbox` profiles.

## Profile Integration

Create `internal/profiles/incus-in-incus.yaml` with:

- `devices: {}`;
- an explicit `security.privileged: "false"` setting;
- `security.nesting: "true"` and only any additional settings proven necessary by the live test;
- a description identifying it as an unprivileged profile for nested Incus containers.

Register `incus-in-incus` in the `Type` constants, embedded profile-path map, and lexically ordered fresh slice returned by `Types()`. The generic `profile <type>` Cobra command then supports the profile without command-specific changes.

## Live Validation

Use unique disposable names and perform the following on the local Incus 7.3 server:

1. Launch a bare outer system container using the existing `default` profile.
2. Install Incus inside the outer container.
3. Initialize a minimal nested Incus server using directory-backed storage, avoiding nested VM and advanced storage requirements.
4. Keep the outer container unprivileged and add profile settings incrementally until the nested server can launch an inner system container.
5. Execute a sentinel command inside the inner container to prove it started and is usable.
6. Encode only the settings required by that proof in `incus-in-incus.yaml`.
7. Repeat the test from a fresh outer container using `default + incus-in-incus`, without per-instance configuration overrides.

Any failure is diagnosed before broadening the profile. Privileged mode and raw LXC/AppArmor overrides are outside the accepted design.

## Automated Tests

Extend `internal/profiles/profiles_test.go` to verify:

- `Render` supports `incus-in-incus` and emits its expected description;
- `Types()` includes `incus-in-incus` in lexical order and still returns fresh package state;
- the rendered profile has no devices, remains explicitly unprivileged, enables nesting, and includes any other setting justified by live validation;
- unknown-profile errors enumerate the new supported type.

Run focused profile tests followed by the full Go test suite, race tests, vet, build, and diff checks.

## Cleanup and Failure Handling

Live-test containers use collision-resistant names. Cleanup removes every temporary outer container, which also removes its nested daemon, storage, images, and inner containers. Cleanup runs after success or failure. Existing host profiles, instances, images, networks, storage pools, and the running `operator` instance are not modified. Final verification confirms no temporary test instance remains and the Git worktree contains only intended changes.
