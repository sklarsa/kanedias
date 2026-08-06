# Kanedias CLI Design

## Goal

Provide one small Cobra executable that loads repository-local configuration, renders embedded Incus profiles, manages the configured Incus bridge before proxy startup, and exposes the existing credential proxy through a structured command hierarchy.

## Command Surface

```text
kanedias proxy run
kanedias proxy init-ca
kanedias proxy login openai-codex
kanedias profile <sandbox|image-build|lemonade>
```

The root command has a persistent `--config` flag whose default is `./config.toml`. Cobra's generated commands remain enabled.

`proxy run` retains the existing metrics, request-logging, CA-path, and credential-path flags. The public `--listen` flag is removed: the command always binds to the configured IPv4 address on port `3128`. The internal proxy options retain a configurable listen address for automated tests.

`proxy init-ca` accepts the applicable CA path flags. `proxy login openai-codex` accepts the applicable OpenAI Codex authentication path flag. These non-listening operations do not inspect or modify Incus networking.

## Internal Packages

### `internal/config`

This package owns the TOML representation and loading logic. Its public configuration structure contains a network section with:

- required `ipv4`, represented in TOML as an IPv4 prefix such as `10.76.111.1/24`;
- optional `ipv6`, represented as an IPv6 prefix when present.

Loading rejects unreadable TOML, missing IPv4, invalid prefixes, an IPv6 value in the IPv4 field, or an IPv4 value in the IPv6 field. It exposes parsed addresses and prefixes needed by the network, profile, and command packages.

### `internal/network`

This package ensures that the fixed Incus network named `kanedias` exists. It invokes the Incus CLI through a small injectable command runner so behavior can be tested without a live daemon.

If the network is absent, `Ensure` creates it as a managed bridge with the configured IPv4 address. It adds the configured IPv6 address only when IPv6 is present in the TOML.

If the network already exists, `Ensure` checks that it is a managed bridge and that its IPv4 prefix is equivalent to the configured prefix. When IPv6 is configured, it also requires an equivalent IPv6 prefix. When IPv6 is omitted, existing IPv6 configuration is not enforced. A mismatch produces a clear error; the command never mutates an existing network.

Only `kanedias proxy run` calls `Ensure`, immediately before starting the listener.

### `internal/profiles`

This package embeds the `sandbox`, `image-build`, and `lemonade` profile templates with `go:embed`. It exposes a render operation selected by the three stable type names.

The sandbox template derives all uppercase and lowercase HTTP/HTTPS proxy environment values from the configured IPv4 address:

```text
http://<configured-ipv4-address>:3128
```

The other profile templates render without config-dependent substitutions. Rendering an unknown type fails with an error that lists the supported values. Successful rendering writes profile YAML without informational text, making stdout suitable for piping into `incus profile edit`.

### `internal/proxy`

The current proxy implementation moves from executable package `proxy` into this importable internal package. It exposes option defaults and operations for running the listener, initializing the CA, and performing OpenAI Codex login. Operations return errors instead of terminating the process.

Existing proxy behavior, credential handling, CA generation, request logging, and metrics remain unchanged. Listener address remains an internal option so tests can use an ephemeral or loopback address, while the CLI supplies the configured network address.

### `cmd` and `main.go`

The `cmd` package constructs the Cobra hierarchy, owns flag binding, loads configuration, and coordinates package calls. The repository-root `main.go` invokes `cmd.Execute()` and converts a returned error into process exit status.

The command layer performs orchestration only:

- `profile`: load config, validate the requested type, render to stdout;
- `proxy run`: load config, ensure the Incus bridge, run the proxy on `<ipv4-address>:3128`;
- `proxy init-ca`: initialize or load the CA without network setup;
- `proxy login openai-codex`: perform login without network setup.

## Error Handling

Configuration errors are reported before any side effects. Incus-not-found errors, bridge lookup or creation failures, bridge mismatches, template failures, CA failures, login failures, and listener failures propagate through Cobra as concise errors written to stderr with a nonzero process status.

No internal package calls `os.Exit`. Normal profile output is isolated on stdout. The long-running proxy continues to use structured logging on stderr.

## Testing

Tests will cover:

- `internal/config`: valid IPv4, optional IPv6, missing or malformed IPv4, malformed IPv6, and TOML errors;
- `internal/profiles`: all three types, unknown types, and config-derived sandbox proxy variables;
- `internal/network`: creation when absent, acceptance of a matching bridge, rejection of unmanaged or wrong-type networks, address mismatches, optional IPv6 behavior, and Incus command failures using a fake runner;
- `cmd`: command hierarchy, argument validation, profile stdout, and confirmation that only `proxy run` performs network setup;
- `internal/proxy`: the existing proxy suite after the package move.

Verification consists of `go test ./...` plus smoke tests that render the three CLI profile types.

## Scope Boundary

Existing shell scripts and shell test harnesses are not modified. Moving `profiles` and `proxy` under `internal` intentionally leaves their current direct paths stale until the planned follow-up migration converts those scripts to invoke the CLI. No unrelated script cleanup or Incus workflow migration is included here.
