# Configurable Image Build Scripts Design

## Summary

Kanedias image creation currently installs both the software required by Kanedias and one operator's broader development environment from a single embedded installer. Split those responsibilities:

1. Keep a small embedded installer containing the Kanedias runtime and lightweight general development utilities.
2. Run operator-provided build scripts from a directory configured in `config.toml` after the core installation succeeds.
3. Move the current heavyweight and personal additions into repository-local numbered scripts.
4. Remove Claude Code and the custom Pi theme rather than moving them.

After implementation and verification, rebuild the configured Incus image.

## Goals

- Make the published image usable by Kanedias even when no custom build scripts are configured.
- Keep operator-specific tools and configuration out of the embedded Kanedias installer.
- Support one or more root-run scripts with deterministic ordering and streamed output.
- Fail before publication when script discovery, upload, or execution fails.
- Preserve the existing temporary-instance cleanup behavior.

## Non-goals

- A general artifact-copying facility for custom scripts.
- Recursive script discovery.
- Per-script users, environments, timeouts, or failure policies.
- Continuing to install Claude Code or the custom Pi theme.

## Configuration

Add an optional field under `base_image`:

```toml
[base_image]
build_scripts_dir = "image-build.d"
```

An absolute value is used as written. A relative value resolves from the directory containing the loaded `config.toml`, matching the repository's existing config-relative asset behavior. Omitting the field means that no custom scripts run.

The repository's `config.toml` will point to a new `image-build.d` directory containing the extracted operator additions.

## Script Discovery

Image build input loading discovers scripts before connecting to Incus. This preserves the existing guarantee that local input errors do not leave a temporary build instance behind.

Discovery examines only direct children of the configured directory:

- Select names ending in `.sh` whose regular-file mode has at least one execute bit.
- Ignore non-`.sh` entries and non-executable regular `.sh` files.
- Reject a `.sh` entry that is executable but is a symlink or another non-regular file.
- Sort selected scripts lexically by filename.
- Read every selected script during preflight.

A configured directory that is missing, unreadable, or not a directory is an error. An existing directory with no selected scripts is valid and behaves as a no-op.

## Image Build Flow

The build sequence is:

1. Validate lifecycle configuration and load all embedded assets and configured scripts.
2. Create the temporary Incus instance and upload the embedded Kanedias inputs.
3. Run the embedded core installer.
4. Verify the embedded Pi extension's production dependencies.
5. Create `/root/build-scripts` in the temporary instance.
6. Upload selected scripts there with mode `0700`.
7. Execute each uploaded script directly as root in lexical filename order, streaming stdout and stderr through the image command's existing writers.
8. Stop and publish the instance only after every script succeeds.

Each script is a separate process. Filesystem and package changes persist between scripts, but shell variables and functions do not. Direct execution honors each script's shebang. Custom scripts may rely on the completed core installation, including Debian 13, the `kanedias` user, NVM, Node, Pi, Git, GitHub CLI, and the standard small utilities.

Any directory creation, upload, or script execution failure identifies the relevant operation or filename, aborts publication, and uses the existing detached, bounded cleanup path for the temporary instance.

## Embedded Core Boundary

The embedded installer retains only:

- Creation/configuration of the `kanedias` UID/GID 1000 account, authorized SSH keys, passwordless sudo, and GitHub HTTPS credential helper.
- Git, GitHub CLI, OpenSSH client/server, curl, CA certificates, NVM, Node 22, and the Pi coding agent.
- Pi authentication, model definitions, and basic settings needed to launch sessions.
- The embedded Kanedias Pi extension and skills, production npm dependencies, protected runtime directories, environment bridge, RPC launcher, systemd socket, and service.
- Lightweight general development utilities from the current base package list, including archive, file, network, process, shell, editor, and inspection tools such as `fd`, `jq`, `make`, `python3`, `ripgrep`, `shellcheck`, `tmux`, `vim`, and `zsh`.

GCC and Clang move to custom scripts. The redundant Debian `nodejs` package is removed because the core installs Node 22 through NVM. Other lightweight packages from the current initial apt installation remain in core.

The core no longer installs or configures:

- Azure, Google Cloud, or AWS command-line tooling.
- AWS Session Manager plugin.
- Docker, Podman, Buildx, Compose, k9s, or kind.
- GCC, Clang, Go, Pulumi, uv, or tfenv.
- Superpowers, `pi-web-suite`, or OpenAI Fast.
- Personal tmux settings.
- Claude Code.
- The custom Pi theme.

## Custom Script Layout

Use numbered scripts under `image-build.d` to keep concerns isolated and ordering explicit:

- Cloud tools: Azure CLI, Google Cloud CLI, AWS CLI, and AWS Session Manager plugin.
- Container tools: Docker, Podman, Buildx, Compose, k9s, and kind.
- Development toolchains: GCC, Clang, Go, Pulumi, uv, and tfenv.
- Pi extras: Superpowers, `pi-web-suite`, and OpenAI Fast, installed as the managed user; this script also enables OpenAI Fast.
- Personal configuration: the two existing tmux settings.

The scripts retain the current install behavior except for the deliberate removals and boundary changes above. They are self-contained and may use `runuser` when an operation must run as `kanedias`.

## Pi Asset Changes

- Remove the custom theme file from image inputs and delete its installation logic.
- Remove the theme and operator package list from the core `pi-settings.json`.
- Remove core creation of the OpenAI Fast enablement file.
- Have the Pi extras script install/register Superpowers, `pi-web-suite`, and OpenAI Fast and create OpenAI Fast's enablement file.
- Stop treating the personal tmux file as an embedded core input; the personal-configuration script writes those settings instead.

Pi authentication and models remain core inputs because managed sessions require them to connect to the configured model provider.

## Testing and Verification

Add or update hermetic tests for:

- TOML decoding of `base_image.build_scripts_dir`.
- Config-relative and absolute directory resolution.
- Omitted script configuration.
- Missing/unreadable/not-directory failures before Incus connection.
- Direct-child selection, executable filtering, non-regular rejection, and lexical ordering.
- Script uploads and execution after the core installer and extension verification but before instance stop/publication.
- Streamed script output.
- A named script failure preventing publication while preserving cleanup.
- Heavyweight and personal installs being absent from the embedded installer and present in the appropriate custom scripts.
- Removed Claude Code, theme, and redundant core package/config references.

Run:

```bash
gofmt -w .
bash -n internal/image/install.sh image-build.d/*.sh
shellcheck internal/image/install.sh image-build.d/*.sh
go test ./...
node --test internal/server/web/*.test.js
golangci-lint run ./...
```

After all checks pass, rebuild and publish the configured image:

```bash
make build
./bin/kanedias --config config.toml image create
```

A successful final report must include the verification results and the image publication result. If image rebuilding fails because of an external service, package repository, network, or Incus condition, report the failure and do not claim the image was rebuilt.
