# Container Lifecycle Housecleaning Design

## Goal

Make the supervisor/session flow the only supported way to launch Kanedias containers. Remove the obsolete manual sandbox lifecycle and the incomplete native nested-Incus proof of concept rather than maintaining two competing launch paths.

This design record is temporary: the approved cleanup removes all `docs/superpowers/**`, including this file, from the final tree.

## Scope

Remove the following production surfaces and their tests:

- the `sandbox create` and `sandbox destroy` commands;
- the `internal/sandbox` package;
- the `workspace incus sync` command;
- the `internal/workspace/incus` package and its live test;
- nested-Incus configuration under `workspace.incus`;
- nested-Incus image setup (`incus-base` and `incus-admin` membership);
- profile settings added only for nested Incus;
- nested-Incus-only Incus client helpers, environment flags, comments, and test cases;
- all historical `docs/superpowers/**` plans and specifications.

Preserve:

- supervisor-managed root and child session provisioning;
- `workspace repos sync`;
- the sandbox profile name used by supervisor-managed session containers;
- general unprivileged-container and nesting settings needed for Docker/kind;
- shared Incus client helpers still used by supervisor provisioning.

## Behavior and Validation

The CLI will no longer expose `sandbox` or `workspace incus`. Tests will assert the remaining command hierarchy and preserve coverage for `workspace repos sync` and supervisor/session behavior.

Validation will include Go formatting, the hermetic test suite, a build, tagged compilation without executing destructive live tests, relevant shell validation, and lint when the configured tool is available. Repository-wide searches will confirm no production nested-Incus wiring remains.

## Deferred Work

GitHub issue #21 will become the restoration tracker. It will explain why the proof of concept was removed and require a future design to address:

- reliable Incus daemon initialization and required packages;
- unprivileged nesting failures involving BPF and user-namespace mappings;
- explicit storage mount, ownership, copy-on-write, and isolation semantics;
- live/CI validation of recursive Btrfs behavior;
- local-model proxy propagation;
- integration with the single supervisor-owned container lifecycle rather than a second manual command path.

## Delivery

Commit the cleanup on `rm-sandbox`, push it, open a pull request against `main`, update issue #21 with the final PR reference, wait for required checks, and merge the approved PR.
