# Kanedias Session Spike Design

## Goal

Add a one-shot command that proves Kanedias can run one Pi process in one Incus sandbox and expose Pi RPC over a production-shaped network boundary:

```text
<prompt on stdin> | kanedias session
```

The command creates an ephemeral sandbox, connects to Pi RPC over TCP, sends one prompt, streams the complete raw Pi RPC JSONL response to stdout, and deletes the sandbox and its cloned workspace.

This is a focused spike toward a future Kanedias server with an integrated credential proxy and web UI. It does not build that server or a general session framework.

## Command Contract

`kanedias session` accepts no positional prompt. It reads the complete prompt from stdin and rejects empty input before creating resources.

Stdout contains only the raw LF-delimited JSON records emitted by Pi RPC. This includes command responses, text and thinking deltas, tool events, and lifecycle events. Kanedias lifecycle progress and errors go to stderr so stdout remains machine-readable.

The existing credential proxy is an external prerequisite for workspace synchronization and sandbox/session traffic. `kanedias proxy run` must already be listening at the configured sandbox proxy endpoint before `workspace sync` or `session`; neither command starts or owns the proxy. Base-image construction uses direct IPv4 egress through the managed bridge and does not use or require the Kanedias proxy.

The command sends exactly one RPC request:

```json
{"id":"prompt-1","type":"prompt","message":"..."}
```

It forwards records until Pi emits `agent_settled`, then closes the connection and removes the session resources.

## Hardcoded Incus Project

Kanedias images, profiles, instances, and custom volumes live in the hardcoded Incus project `kanedias`. The Incus-managed bridge remains in the default network project and is shared with `kanedias` through `features.networks=false`, because bridge networks cannot be project-local. Network operations still flow through the project-scoped client, and Incus maps them to the default network project. The bridge requires `ipv4.nat=true` so temporary image-build containers have direct package-download egress; an existing bridge without that setting fails with an actionable manual correction rather than being mutated automatically.

Connection setup will:

1. connect to the local Incus daemon;
2. create `kanedias` when it is absent;
3. enable isolated images, profiles, and storage volumes while setting `features.networks=false` on a newly created project;
4. validate those exact required feature values when the project already exists;
5. return a client scoped with `UseProject("kanedias")`.

Existing resources in the default project are not migrated or deleted. Once this change lands, the base image and workspace seed must be recreated in `kanedias`, while the existing managed bridge remains in the default network project. Migration tooling is outside the spike.

## Incus-Owned Session State

The Incus instance is the session record. The command generates a unique instance name, which acts as the session ID, and records discoverable metadata in instance configuration:

```text
user.kanedias.kind=session
user.kanedias.rpc.port=7777
```

Incus provides the instance lifecycle state, creation metadata, network address, profiles, and attached workspace device. The cloned custom volume is named from the session instance and is owned only by that invocation. No additional state file or application database is introduced.

## Guest RPC Endpoint

Pi RPC itself is a strict JSONL protocol over stdin and stdout; Pi does not open a network listener. The base image will use systemd's inetd-style socket activation to attach an accepted TCP connection directly to those file descriptors.

A socket unit will listen on TCP port `7777` inside the private `kanedias` network:

```ini
[Socket]
ListenStream=0.0.0.0:7777
Accept=yes
MaxConnections=1
NoDelay=yes
```

A matching service template will run as the existing `kanedias` user with `/workspace` as its working directory:

```ini
[Service]
User=kanedias
Group=kanedias
Environment=HOME=/home/kanedias
WorkingDirectory=/workspace
ExecStart=/usr/local/libexec/kanedias-pi-rpc
StandardInput=socket
StandardOutput=inherit
StandardError=journal
```

`StandardInput=socket` connects Pi stdin to the accepted socket. `StandardOutput=inherit` sends Pi stdout back over the same socket. Stderr goes to journald so diagnostics cannot corrupt the RPC stream.

A small shell launcher will load the image's NVM installation and replace itself with:

```text
pi --mode rpc --no-session
```

The image installer will install the launcher and units and enable the socket. This introduces no guest bridge binary and no `socat` or `netcat` dependency. The session command will not use Incus exec to launch or communicate with Pi.

## Session Workflow

A focused `internal/session` package will own the workflow:

1. Validate the configuration and prompt before side effects.
2. Connect to the project-scoped Incus client.
3. Initialize the existing proxy CA, ensure the managed network, and ensure the sandbox profile.
4. Resolve the storage pool and verify the base image and workspace seed prerequisites.
5. Copy the seed into a uniquely named session workspace volume.
6. Create the session instance from the configured base image with the `default` and `sandbox` profiles, its owned workspace device, and the session metadata.
7. Start the instance.
8. Poll Incus instance state until `eth0` has an IPv4 address.
9. Dial the metadata-defined RPC port, retrying connection refusal until a fixed readiness timeout expires. The successful connection activates Pi through systemd.
10. Send the prompt command and read strict LF-delimited records.
11. Write every complete record to stdout unchanged while inspecting only the minimal fields needed to detect prompt rejection and `agent_settled`.
12. Close the connection and clean up the instance and volume.

The TCP connection is the only Pi RPC transport. Incus APIs remain responsible for resource lifecycle and address discovery.

## Errors and Cleanup

The implementation will keep error handling proportional to a spike. It will report concise failures for invalid input, missing or incompatible project prerequisites, unavailable proxy or RPC endpoints, rejected prompts, malformed JSONL, and connection closure before settlement.

A fixed startup timeout bounds address and RPC-port readiness. There is no overall model-response timeout; the command runs until Pi settles or the command context is cancelled.

Cleanup runs after success, failure, output failure, or cancellation. It uses a fresh 30-second context derived with `context.WithoutCancel`, stops and deletes only the instance created by the invocation, and then deletes only its cloned workspace volume. Creation flags prevent collision failures from deleting pre-existing resources. Cleanup failures are joined with the primary error.

The RPC port has no application authentication in this spike. It is limited to the private Incus network. Authentication, reconnection, and replay belong to the future server design.

## Testing

Testing remains intentionally narrow:

- project creation and project-scoped client selection;
- Cobra stdin reading and session delegation;
- one happy-path session lifecycle using a narrow fake Incus client;
- a local TCP fake that verifies the prompt request, byte-for-byte JSONL forwarding, and completion on `agent_settled`;
- cleanup after one representative startup or session failure;
- managed-network assertions for direct IPv4 NAT on creation and validation;
- image-installer assertions for the installed and enabled systemd socket;
- the existing `go test ./...` suite;
- one live smoke test with the proxy running:

```bash
echo 'Reply with a short greeting.' | kanedias session
```

## Explicit Non-Goals

This spike does not add:

- reconnect or event replay;
- multiple prompts or concurrent clients;
- a custom guest daemon;
- authentication on the private RPC port;
- comprehensive interpretation of Pi RPC events;
- preservation of completed or failed session sandboxes;
- migration of default-project resources;
- a Kanedias HTTP server, proxy supervisor, or web UI.
