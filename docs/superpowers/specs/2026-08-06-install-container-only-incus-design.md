# Container-Only Incus Image Installation Design

## Goal

Install the Debian 13 container-only Incus daemon and client in the published Kanedias image without initializing nested Incus or adding another host-side Incus profile.

## Installer Changes

Add `incus-base` to the initial `apt-get install --no-install-recommends` package list in `internal/image/install.sh`. Debian 13's official repository already provides the package, so the installer adds no signing key, package source, or external repository. `incus-base` depends on `incus-client` and contains the daemon support required for system and OCI containers without the QEMU dependencies pulled in by the full `incus` metapackage.

After package installation has created the `incus-admin` group, `configure_managed_user` adds the existing `kanedias` user to both `sudo` and `incus-admin`. Incus daemon access is root-equivalent inside the outer container, but this does not add a new trust boundary because `kanedias` already has passwordless sudo there.

The installer does not run `incus admin init`, create a storage pool or network, invoke the Incus client, or explicitly start/enable Incus services. Each launched container therefore retains an uninitialized nested daemon until its user chooses a configuration. Existing host profiles and the `default + image-build` image construction flow remain unchanged.

## Repository Cleanup

Delete the abandoned `2026-08-06-incus-in-incus-profile` design and plan documents. Do not add an `incus-in-incus` profile or modify the existing `image-build` and `sandbox` profiles.

## Validation

Add a focused test over the embedded installer artifact that verifies:

- the initial Debian package batch includes `incus-base`;
- it does not install the full `incus` metapackage;
- `configure_managed_user` grants `incus-admin` membership;
- the script contains no `incus admin init` invocation.

Run the focused image tests, `shellcheck internal/image/install.sh`, the full Go test and race-test suites, vet, build, and diff checks.

For live proof, rebuild the published image through the existing image workflow, launch a uniquely named disposable container with the existing profiles, and verify that `incus-base` is installed, `kanedias` belongs to `incus-admin`, the Incus client can reach the socket-activated daemon, and no storage pool exists before user initialization. Always remove the disposable test container and confirm no image-build or validation instance remains.
