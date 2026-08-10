# Proxy CA Image Trust Design

**Status:** Approved

**Date:** 2026-08-10

## Purpose

Kanedias Pi sessions correctly inherit `HTTP_PROXY`, `HTTPS_PROXY`, the GitHub placeholder token, and CA-related environment variables. However, the sandbox profile currently mounts the proxy CA at `/usr/local/share/ca-certificates/kanedias-proxy.crt` only after the base image has been built. Nothing regenerates Debian's `/etc/ssl/certs/ca-certificates.crt` bundle after that mount. Consequently, intercepted GitHub TLS fails in `curl`, `gh`, and Git with an unknown-authority error even though those clients correctly select the proxy.

The sandbox image must trust the same public CA certificate used by the host credential proxy. The private CA key must remain host-only.

## Scope

This change will:

- ensure the persistent proxy CA exists before image-build resources are created;
- include only the public CA certificate in the image build inputs;
- install it into Debian's local CA directory and run `update-ca-certificates` before publishing the sandbox image;
- remove the runtime host-path CA mount from the sandbox profile;
- retain `NODE_EXTRA_CA_CERTS=/usr/local/share/ca-certificates/kanedias-proxy.crt` and `SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt`;
- rebuild the real sandbox image and validate a fresh supervised Pi session end to end.

This change will not:

- copy or expose the proxy CA private key;
- rebuild images automatically when `proxy run` starts;
- add SSH tunneling or rewrite SSH-style Git remotes;
- turn the shared proxy into a daemon or change its lifecycle;
- modify or migrate active sandbox instances.

## Chosen Architecture

`image.Create` will use the proxy package's default CA paths, call `proxy.InitCA`, and read the resulting public certificate before connecting to Incus or creating an image-build instance. The certificate becomes an explicit `buildInputs` member. This keeps CA generation in the existing proxy implementation and makes image creation fail before Incus side effects if the CA cannot be initialized or read.

The image workflow uploads the certificate as `/root/assets/kanedias-proxy.crt`. `internal/image/install.sh` treats it as a required input, installs it at `/usr/local/share/ca-certificates/kanedias-proxy.crt` with mode `0644`, and runs `update-ca-certificates`. Debian's standard system bundle therefore contains the Kanedias CA when the image is published.

The runtime `proxy-ca` disk device will be removed from `internal/profiles/sandbox.yaml`. Session containers use the certificate and generated bundle embedded in their immutable root filesystem. The host retains the CA key and the credential proxy uses that same keypair.

## Alternatives Considered

### Runtime systemd trust setup

A privileged unit could run `update-ca-certificates` after the host certificate is mounted into every new session. This handles CA rotation without rebuilding the image, but introduces another boot-order dependency and does not automatically cover commands run before that unit completes. It is unnecessary for a persistent, rarely rotated CA.

### Runtime combined bundle

A pre-start helper could concatenate the system roots and mounted proxy CA into `/run` and point Pi at it. This would fix Pi descendants but not all container processes or image/workspace preparation commands. It would also duplicate Debian's trust-bundle management.

### Proxy-triggered image rebuild

`proxy run` could rebuild and republish the sandbox image when it generates a CA. This couples a lightweight shared service to an expensive, stateful image publication operation and could race with session creation. Image publication remains an explicit operator action.

## CA Lifecycle

The proxy CA is persistent: `proxy.InitCA` creates it only when both certificate and key are absent. Normal proxy restarts therefore use the certificate already baked into the image.

Intentional CA rotation requires this order:

1. stop the proxy and active sessions that depend on it;
2. replace or regenerate the host CA keypair;
3. run `kanedias image create` to publish a sandbox image containing the new public certificate;
4. restart the proxy;
5. start new sessions.

This explicit rebuild requirement prevents a runtime host mount from silently replacing the certificate while leaving the system trust bundle stale.

## Failure Handling

- Failure to resolve the user configuration directory, initialize the CA, or read its certificate aborts image creation before Incus resources are created.
- Failure to upload or install the certificate aborts image publication and uses the existing image-build cleanup path.
- Failure of `update-ca-certificates` aborts image publication.
- Session provisioning retains its existing fail-fast TCP proxy reachability check.
- Existing published images remain unchanged if a new image build fails.

## Test Strategy

### Hermetic tests

Tests will prove that:

- image input loading initializes and reads the public CA from an isolated configuration directory;
- the image workflow uploads the public certificate but never the private key;
- the installer requires the CA input, installs it at the expected path, and invokes `update-ca-certificates` before publication;
- the rendered sandbox profile retains proxy/CA environment variables but no longer contains the runtime `proxy-ca` disk device or host CA path.

Each production change will follow a failing-test-first cycle.

### Live end-to-end acceptance

Using the current branch binary and configured Incus environment:

1. rebuild and publish the `sandbox` image;
2. start an owned credential proxy and wait for `10.76.111.1:3128` readiness;
3. start a disposable root supervisor and wait for `/v1/tree` lifecycle `ready`;
4. verify the image certificate matches the host public CA and validates against `/etc/ssl/certs/ca-certificates.crt`;
5. verify the actual systemd Pi process contains the expected proxy and CA variables;
6. verify a generic HTTPS tunnel and intercepted GitHub HTTPS with certificate verification enabled;
7. verify authenticated `gh` REST, GraphQL, and private-repository access;
8. verify HTTPS Git `ls-remote`, private clone, fetch, and `push --dry-run`;
9. verify the dry-run created no remote ref;
10. gracefully delete the disposable session and confirm its Incus resources disappear;
11. stop only the proxy process owned by the test.

The live test will redact credentials, perform no real GitHub mutation, preserve unrelated sessions, and leave the newly published sandbox image as the intended output.
