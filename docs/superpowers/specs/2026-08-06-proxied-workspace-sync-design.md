# Proxied Workspace Sync Design

## Goal

Make `sync-workspace.sh` populate the workspace seed through the sandbox credential proxy, using GitHub CLI over HTTPS and waiting for DNS readiness before repository access.

## Instance setup

The sync script will initialize the proxy CA, create or refresh the Incus `sandbox` profile from `profiles/sandbox.yaml`, and initialize the temporary instance with both the `default` and `sandbox` profiles. It will override the inherited `workspace` device to honor the selected storage pool and volume, preserving the existing environment overrides.

After startup, the script will poll `getent ahosts github.com` inside the instance for up to 60 seconds. A timeout will fail with a clear error and trigger the existing cleanup. Once DNS works, the script will update the instance CA trust before accessing GitHub. The long-running proxy remains externally managed.

## Repository sync

`private/repos.txt` will contain `owner/repository` slugs. The sync process will:

1. Configure GitHub CLI as Git's credential helper for `github.com`.
2. Derive `https://github.com/owner/repository.git` for each slug.
3. Clone missing repositories with `gh repo clone` using that explicit HTTPS URL.
4. Replace existing `origin` URLs with the derived HTTPS URL before fetching and resetting.

Explicit HTTPS ensures Git and recursive submodule operations use the profile's proxy. Existing workspace repositories migrate in place on their next sync.

## Validation and errors

Repository slugs must have exactly one non-empty owner/repository separator and must still produce unique destination names. Invalid slugs, duplicate destinations, DNS timeout, profile setup failure, CA update failure, authentication failure, or Git failure aborts the run and invokes existing temporary-instance cleanup.

Tests will cover slug validation, `gh` cloning, HTTPS origin migration, DNS retry and timeout behavior, sandbox profile application, workspace override ordering, and cleanup. Existing idempotency and repository-safety tests remain in force.
