# Per-Session Model Profiles Design

**Status:** Approved

**Date:** 2026-08-09

## Purpose

Kanedias currently creates a root session without launch options. The root uses the model defaults baked into the Pi image, while every descendant resolves its worker profile from the process-wide `config.toml`. Operators need to choose a root model when creating a session, customize the model and thinking level for each subagent role, and have every nested clone inherit those choices for the lifetime of the session tree.

This design adds an admin-approved model allowlist, a focused New Session modal, and an immutable per-tree model policy.

## Goals

- Require every server-created root to have an explicitly represented root model selection.
- Preselect configured defaults so a standard launch remains one click.
- Allow per-session model and thinking-level choices for the root and every configured worker role.
- Keep worker names, descriptions, and purposes under global administrator policy.
- Restrict all choices to an administrator-configured model allowlist.
- Start root Pi on the selected model rather than switching it after startup.
- Make the complete selected worker map immutable and inherited by every nested clone.
- Keep active tree behavior stable if global model defaults later change.
- Show the model Pi actually bound for each root and child.
- Reject invalid configuration or launch input before creating a process or Incus resource.

## Non-goals

- Persisting a session's choices as new global defaults.
- Remembering the last browser selection.
- Free-form provider or model entry.
- Creating, renaming, or editing worker roles in the browser.
- Child-specific overrides of the policy inherited from the root.
- Changing model policy after a tree has started.
- Hot-reloading the server's launch catalog.
- Storing model credentials in the launch policy.

## Configuration

### Model allowlist

`config.toml` gains a model catalog keyed by stable, operator-facing model type IDs:

```toml
[models.local-qwen]
label = "Qwen 3.6 27B (Local)"
provider = "local-executor"
model = "Qwen3.6-27B-GGUF"
thinking_levels = ["off"]
default_thinking_level = "off"

[models.gpt-5-6-sol]
label = "GPT-5.6 Sol"
provider = "openai-codex"
model = "gpt-5.6-sol"
thinking_levels = ["off", "minimal", "low", "medium", "high", "xhigh", "max"]
default_thinking_level = "high"
```

A model type contains no credentials. Provider authentication remains in the existing Pi image assets and credential proxy flow.

Model type IDs must match `^[a-z0-9][a-z0-9-]{0,62}$`, making them safe, stable slugs for typed requests and HTML bindings. Provider/model pairs must be unique so defaults and runtime profiles map to one unambiguous allowlist entry. Labels, providers, model IDs, supported thinking levels, and the model's default thinking level are required. The default thinking level must be one of that model's supported levels.

### Root and worker defaults

The root and existing workers reference model type IDs rather than duplicating provider/model pairs:

```toml
[session]
model_type = "local-qwen"
thinking_level = "off"

[workers.researcher]
description = "Research external sources and return evidence without modifying files."
model_type = "gpt-5-6-sol"
thinking_level = "high"

[workers.reviewer]
description = "Review code and designs without modifying files."
model_type = "gpt-5-6-sol"
thinking_level = "xhigh"

[workers.worker]
description = "Implement changes and hand off pushed Git refs."
model_type = "gpt-5-6-sol"
thinking_level = "high"
```

Configuration loading resolves these references into runtime provider/model/thinking profiles. Validation fails when a default names an unknown model type or uses a thinking level unsupported by that model. Existing worker names and descriptions remain the source of role policy.

Direct `kanedias session --socket ...` launches use these configured defaults when no server bootstrap is supplied.

## Domain Types

The browser and server exchange allowlist identifiers, never raw provider/model values:

```text
SessionLaunchRequest
  Root: ModelSelection
  Workers: []WorkerModelSelection

ModelSelection
  ModelType: string
  ThinkingLevel: string

WorkerModelSelection
  WorkerType: string
  ModelType: string
  ThinkingLevel: string
```

After validation, the manager resolves that request into an immutable runtime snapshot:

```text
SessionModelPolicy
  Root: ModelProfile
  Workers: map[workerType]SessionWorkerProfile

ModelProfile
  Provider: string
  Model: string
  ThinkingLevel: string

SessionWorkerProfile
  Description: string
  Provider: string
  Model: string
  ThinkingLevel: string
```

The policy contains the complete worker map. A partial worker list is invalid. The request must contain every configured worker role exactly once: missing, duplicate, and unknown roles are rejected. The wire format uses a list so duplicate roles can be detected rather than silently overwritten during JSON object decoding.

The policy is copied at boundaries so a caller cannot mutate a running tree through a retained map reference.

## New Session Modal

Clicking **New Session** opens a centered, accessible modal matching the existing Astrolabe visual language.

The modal contains:

1. an always-visible root model selector;
2. an always-visible root thinking-level selector;
3. an expandable **Subagent model profiles** section;
4. one model/thinking row for each configured worker role;
5. Cancel and Launch Session actions;
6. an inline status region for launch validation or admission errors.

Every field is preselected from `config.toml`. Opening the modal again resets it to those defaults; it does not retain the previous session's choices.

Changing a model updates that row's thinking selector to the model's supported levels. If the current level is unsupported, the selector moves to the model's configured default thinking level. A one-level model renders a disabled selector with that level visible.

Cancel, the close control, and Escape close the modal without a request. Launch Session disables while the request is pending to prevent accidental duplicate submissions. A failed request leaves the modal open and restores the action. Successful admission closes and resets the modal; the existing fleet stream then renders the admitted root.

The modal is server-rendered from the manager's validated catalog and defaults. No browser-supplied label, provider, model ID, worker description, or role definition is trusted.

## Server and Manager Flow

The manager receives the validated launch catalog and configured defaults at construction. It exposes a read-only launch view for the server and changes root creation to accept a `SessionLaunchRequest`.

The flow is:

1. The server renders the hidden modal from the manager's launch view.
2. The browser submits model type IDs and thinking levels to `POST /ui/sessions`.
3. The server performs bounded, strict request decoding.
4. The manager validates the complete request against its catalog and fixed worker-role set.
5. The manager resolves the request to a `SessionModelPolicy` before generating a spawn token, opening a log, creating a socket path, or starting a process.
6. The manager starts the root supervisor and supplies the policy through a short-lived inherited bootstrap pipe.
7. The root resolves its worker catalog from the policy and provisions Pi with the policy's root model.
8. Admission proceeds through the existing readiness and tree-validation path.

The root bootstrap is a single bounded, strict JSON record. It is not placed in argv, the environment, or a durable generated configuration file. The manager closes its pipe endpoints on success and every failure path. A direct CLI root without a bootstrap builds the same policy type from configured defaults.

## Root Model Binding

The root provision request carries the resolved root `ModelProfile`. Root instance configuration sets the existing `KANEDIAS_PI_PROVIDER`, `KANEDIAS_PI_MODEL`, and `KANEDIAS_PI_THINKING` environment values. The image launcher passes them to Pi as `--provider`, `--model`, and `--thinking`, using the same allowlisted thinking-level validation as child sessions.

Root startup does not launch on the image default and then call a model-changing RPC. The selected model is present from Pi process creation.

After `get_state`, the local session compares Pi's reported provider, model, and thinking level with the expected root profile. A mismatch is a startup invariant failure and triggers the existing bounded root cleanup. Fresh and forked children perform the same comparison against their selected worker profile.

## Clone Inheritance

The existing child bootstrap gains the complete `SessionModelPolicy`.

When any node creates a child:

1. `workerType` is resolved only from that node's immutable policy.
2. The unchanged complete policy and child request are copied into the child bootstrap; there is no second independently mutable worker-profile field.
3. The child resolves its selected profile from the inherited policy for provisioning and expected-model binding.
4. The child constructs its worker catalog from the inherited policy.
5. Any grandchild repeats the same process.

Descendants validate the inherited payload's structure, required roles, values, and exact selected-worker consistency. They do not re-resolve model choices from current global defaults. This keeps a live tree stable if `config.toml` is edited after root admission.

Global configuration remains necessary for infrastructure settings such as workspace, Incus, and proxy configuration. Only the model policy is snapshotted per tree.

The model-facing `delegate_session` contract remains unchanged. Agents still select a worker type, not an arbitrary model. `GET /v1/workers` reflects the session policy so fork preparation and extension guidance use the inherited target profiles.

## UI Observation

The existing tree snapshots already carry the model Pi reports for each node. The session detail view adds:

- provider;
- model ID;
- thinking level.

These values come from successful Pi `get_state` binding, not from the requested policy. They therefore provide an audit of the effective runtime choice. Child rows may continue using worker names as their primary label; no full policy editor is added after launch.

## Validation and Security

Validation is repeated at trust boundaries:

- configuration validates catalog integrity and defaults at server startup;
- the manager validates every browser launch request before side effects;
- the root bootstrap decoder uses strict bounded JSON and rejects unknown fields;
- child bootstrap decoding validates complete policy structure and worker consistency;
- Pi binding checks the effective model against the expected resolved profile.

The browser cannot submit raw providers, model IDs, descriptions, or credentials. Unknown model types, unknown or missing workers, duplicate workers, and unsupported thinking levels are typed invalid requests.

The existing authenticated-console option and same-origin write boundary continue to protect `POST /ui/sessions`. Error responses use sanitized operator copy while logs retain the underlying cause.

No invalid launch request may create a root process, log file, supervisor socket, Incus volume, or Incus instance.

## Error Handling

- **Invalid catalog/defaults:** server startup fails with a configuration error naming the invalid model type or worker.
- **Malformed launch request:** the modal remains open with stable invalid-request copy.
- **Unsupported selection:** rejection occurs before root spawn and reports that the selected configuration is invalid without exposing internal provider details.
- **Spawn or admission failure:** existing manager cleanup escalation runs; the modal remains open and Launch is re-enabled.
- **Pi model mismatch:** supervisor startup fails and cleans all owned resources.
- **Child policy mismatch:** child startup fails before Pi task execution and follows existing direct-child cleanup and recovery.
- **Configuration change during a live tree:** the active tree continues using its inherited model policy.

## Testing Strategy

Implementation follows test-driven development.

### Configuration tests

- valid allowlist, root defaults, and worker defaults resolve correctly;
- duplicate provider/model pairs fail;
- invalid IDs, empty labels, and empty provider/model values fail;
- invalid, duplicate, or empty thinking-level sets fail;
- model default thinking outside the supported set fails;
- root or worker defaults naming an unknown model type fail;
- root or worker thinking unsupported by its model fails.

### Server tests

- the initial page contains an accessible closed modal and preselected defaults;
- every fixed worker role renders once;
- no raw credential material is rendered;
- model changes expose only allowed thinking levels;
- cancel/Escape controls are wired without submission;
- a valid POST forwards the complete typed request;
- malformed, partial, unknown-worker, and unknown-model requests are rejected;
- errors patch the modal status without closing it;
- success resets/closes the modal while preserving existing fleet streaming and action routes;
- authentication and same-origin write-boundary tests still cover the changed POST.

### Manager and process tests

- validation happens before token, path, log, pipe, or process creation;
- valid policy resolution is deterministic and map copies are immutable;
- root argv identifies the bootstrap descriptor without embedding policy JSON;
- the root bootstrap record is strict, bounded, read exactly once, and all descriptors close on every path;
- admission and failed-spawn cleanup retain their existing ordering.

### Supervisor and provisioning tests

- root provisioning emits the selected provider/model/thinking values;
- the image launcher passes root model arguments and rejects invalid inputs;
- `get_state` binding accepts an exact profile and rejects provider, model, or thinking mismatches;
- child creation resolves from the inherited policy rather than current global defaults;
- a child bootstrap contains the complete policy;
- a grandchild receives an exactly equal policy after the backing global defaults are changed in the test;
- fork sanitization receives the inherited target worker profile;
- snapshots and session detail rendering show the effective model.

### Regression verification

Run focused package tests during development, then:

```text
go test ./...
go test -race ./internal/config ./internal/manager ./internal/server ./internal/supervisor/...
go vet ./...
git diff --check
```

No destructive Incus acceptance run is required without its existing explicit authorization. Existing opt-in live acceptance remains the place to prove real provider availability and root/child launch behavior.

## Acceptance Criteria

The feature is complete when:

1. New Session opens the focused modal with configured defaults.
2. The operator can choose an allowlisted root model and thinking level.
3. The operator can expand worker profiles and choose an allowlisted model/thinking pair for each role.
4. Invalid input produces no process or Incus side effect.
5. A valid launch starts root Pi on the selected profile and admits the root.
6. A child uses the selected profile for its worker type.
7. A nested child inherits the same complete profile map.
8. Editing global model defaults after admission does not alter that tree's descendants.
9. Pi's effective provider/model/thinking values appear in session details.
10. Stopping the tree discards its in-memory policy without changing global defaults.
