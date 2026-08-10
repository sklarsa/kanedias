# Session Names and Repository Start Design

**Status:** Approved

**Date:** 2026-08-10

## Purpose

Kanedias currently identifies root sessions in the browser only by their immutable session IDs and starts every Pi process in `/workspace`. Operators need an optional human-readable name for each root, the ability to rename an active root in the GUI, and an optional configured repository as the starting directory for the complete session tree.

A live sandbox inspection also found that `/workspace` is `root:root 0711`, while `/workspace/repos` is `kanedias:kanedias 0755`. Pi runs as `kanedias`, so it cannot write directly under `/workspace`. This design corrects that ownership as part of making workspace startup reliable.

## Goals

- Accept an optional root display name when creating a session.
- Allow an active root to be renamed or have its name cleared from the GUI.
- Keep Kanedias session IDs immutable, visible, and authoritative.
- Allow duplicate display names.
- Keep names in server memory for the initial implementation.
- Accept an optional starting repository selected from configured `workspace.repos`.
- Start root Pi in the selected repository.
- Make fresh and forked descendants inherit the root's selected repository.
- Fail with specific operator-facing copy if the selected checkout is absent or unsafe.
- Make `/workspace` writable by the managed `kanedias` user.
- Repair both workspace seed volumes and roots cloned from older, uncorrected seeds.

## Non-goals

- Persisting display names across a Kanedias server restart. This is tracked by [GitHub issue #57](https://github.com/sklarsa/kanedias/issues/57).
- Changing immutable Kanedias session IDs or Pi session IDs.
- Requiring unique display names.
- Naming or renaming descendants.
- Using display names as Incus instance, volume, socket, log, or route identifiers.
- Accepting arbitrary repository URLs or unconfigured GitHub slugs from the browser.
- Cloning a new repository during session launch.
- Selecting a branch, commit, or subdirectory at launch.
- Silently falling back to `/workspace` when a selected repository is unavailable.

## Responsibility Boundaries

The feature is split at existing ownership boundaries.

### Server and manager

The server and manager own root display names. A name is presentation metadata, does not affect supervision, and does not enter the recursive supervisor API. The manager stores the name on the admitted in-memory root handle and projects it into fleet and session views.

The manager also owns browser launch validation. It exposes the configured repository allowlist in its read-only launch options, validates the selected repository before creating any spawn artifact, and resolves it to a trusted repository start specification.

### Root and child startup

The selected starting repository affects the Pi process working directory, so it crosses the process and provisioning boundary. The root bootstrap and every child bootstrap carry the immutable selected repository. Root and child provisioning validate the corresponding checkout in the cloned workspace and configure Pi to start there.

### Workspace synchronization and provisioning

`workspace repos sync` owns durable seed preparation. It corrects the seed volume's workspace ownership. Root provisioning defensively corrects each cloned root as well, allowing sessions created from an older seed to work immediately. Descendants inherit the corrected root state through existing copy-on-write cloning.

## Domain and Wire Changes

The browser launch request gains two optional fields:

```text
SessionLaunchRequest
  Name: string
  Repository: string
  Root: ModelSelection
  Workers: []WorkerModelSelection
```

`Name` is presentation metadata. `Repository` is either empty or an exact configured `owner/repository` slug.

The read-only launch options gain a sorted repository list for the modal:

```text
SessionLaunchOptions
  Models: []ModelLaunchOption
  Root: ModelSelection
  Workers: []WorkerLaunchOption
  Repositories: []RepositoryLaunchOption
```

Repository options expose only the configured slug and an operator-facing label derived from that slug. They do not expose credentials, filesystem paths, or arbitrary remote URLs.

After validation, the manager resolves the selected repository into an immutable startup value containing the configured slug and expected checkout basename. Only safe structured values cross bootstrap boundaries; the browser never supplies an absolute path.

## Root Display Names

### Validation

Names are optional. The manager trims surrounding whitespace before use. A name must contain no control characters and must be at most 80 Unicode characters. Duplicate names are valid.

An empty name means no custom display name. The immutable root session ID becomes the display-label fallback. Clearing a name through the rename UI restores this fallback.

Launch validation occurs before a spawn token, socket path, log file, bootstrap pipe, process, volume, or instance is created. A name is committed to manager state only when the corresponding root is admitted. Failed launches therefore leave no stale name record.

### In-memory storage

The admitted manager root handle stores the normalized optional display name. `RootState` and `SessionState` project both the display name and immutable root session ID. Root replacement and removal follow the existing handle lifecycle, so in-memory naming metadata is discarded with the handle.

Discovery after a server restart cannot recover the initial implementation's name. Rediscovered roots use their immutable session IDs until renamed again. Durable recovery is deferred to issue #57.

### Rename operation

An authenticated same-origin server endpoint renames a root by immutable session ID. The manager:

1. resolves the target root;
2. applies the same normalization and validation used at launch;
3. updates only the root handle's display name;
4. increments fleet and session revisions; and
5. notifies existing subscribers so the browser updates immediately.

The operation rejects descendant IDs as rename targets. It does not call Pi RPC, mutate a supervisor snapshot, rename resources, or change routing.

### GUI presentation

The New Session modal contains an optional **Session name** text input.

The fleet's root row uses the custom display name when present and the root session ID otherwise. The detail header and root breadcrumb use the same display label. The immutable session ID remains visible in the detail metrics and remains the value used in every DOM route/action attribute.

A root detail header provides an **Edit name** control. Editing uses a bounded text input with Save and Cancel actions. Clearing and saving restores the ID fallback. Child detail pages show the root display label in their breadcrumb but do not expose rename controls.

Rename failures leave the editor open and render sanitized status copy. Successful rename updates the fleet row, detail header, and breadcrumbs through the existing revision/SSE flow.

## Starting Repository

### Launch catalog and modal

The manager derives a deterministic repository launch catalog from configured `workspace.repos`. Repository slugs retain the existing `owner/repository` format and unique destination-basename requirement used by workspace synchronization.

The New Session modal contains an optional repository selector. Its first option represents `/workspace`; remaining options are the sorted configured slugs. Opening or resetting the modal restores the no-repository default. The modal request builder submits the selected slug, never a computed path.

### Validation and resolution

Before any spawn side effect, the manager verifies that a nonempty selection exactly matches the launch catalog. Unknown, malformed, or browser-invented repositories are typed invalid requests.

An empty selection resolves to `/workspace`. A configured slug resolves to `/workspace/repos/<repository-name>`, where `<repository-name>` is the validated unique destination basename from configuration.

The resolved selection is immutable for the life of the session tree. Later configuration edits do not change the startup repository inherited by descendants.

### Bootstrap inheritance

The private root bootstrap carries the resolved repository start specification alongside the model policy. The root command strictly and boundedly decodes it and passes it into supervisor startup.

Every child process bootstrap copies the same repository start specification. Child bootstrap validation checks its structure and safe basename independently. A nested child receives the same value without re-resolving the browser request or silently applying current server defaults.

This explicit inheritance makes root, fresh children, forked children, and nested descendants consistent.

### Workspace validation

After a root or child instance starts but before Pi starts, provisioning validates the selected checkout with argument-array execution inside the instance. For a selected repository, the expected path must:

- exist;
- be a directory and not a symbolic link;
- resolve to the exact literal expected path beneath `/workspace/repos`;
- contain a non-symlink, self-contained Git metadata directory; and
- report the expected path as its Git top level.

The validation treats a missing, replaced, symlinked, escaped, or malformed checkout as unavailable. It never follows a browser-supplied path and never clones or repairs a selected checkout during session launch.

The Pi launcher defaults to `/workspace`. For a selected repository it repeats a minimal defensive directory/containment check, changes directory with safe quoting, and then `exec`s Pi. Pi therefore records and operates from the intended working directory from process creation rather than receiving a later `cd` prompt.

### Missing-repository reporting

Repository absence is detected before Pi task execution and represented by a dedicated typed failure code. Root processes receive a bounded inherited startup-status endpoint in addition to the manager-to-root bootstrap endpoint. On startup failure the root reports only an allowlisted failure code; internal paths and command details remain in server/session logs.

The manager listens for admission and startup status concurrently. The repository-unavailable code maps to the modal copy:

> The selected repository is not present in the workspace.

Other provisioning, process, or admission errors retain the existing generic launch copy. Descendants use the existing child failure protocol, preserving typed failure behavior for delegated sessions.

## Workspace Ownership

The current seed preparation creates or mounts a custom volume whose root is owned by `root`. Repository synchronization fixes only `/workspace/repos`, leaving Pi unable to create files directly in `/workspace`.

`workspace repos sync` will prepare both directories as follows:

```text
/workspace        kanedias:kanedias 0755
/workspace/repos  kanedias:kanedias 0755
```

Preparation runs for newly created and reused seed volumes, including when `workspace.repos` is empty. The empty-repository warning remains, but it occurs only after the mounted volume's ownership has been corrected.

Root provisioning repeats the ownership preparation on the cloned root volume before Pi starts. This defensive repair makes the new behavior effective even when the configured seed predates the fix and has not yet been synchronized again. It changes only the session-owned clone. Descendant COW clones inherit the corrected ownership.

Existing running sessions are not discovered or mutated by this change.

## Data Flow

A successful named repository launch follows this sequence:

1. The server renders the modal from immutable model, worker, and repository launch options.
2. The browser submits optional name, optional repository slug, and complete model selections.
3. Strict bounded JSON decoding rejects malformed or unknown fields.
4. The manager normalizes the name and resolves the repository against its allowlist.
5. The manager resolves the model policy and validates the complete request before side effects.
6. The manager encodes model and repository startup data into the private root bootstrap and starts the root process with a private startup-status endpoint.
7. Root provisioning clones the workspace, corrects `/workspace` ownership, verifies the selected checkout, and configures the Pi working directory.
8. Pi starts in the selected checkout and the root proceeds through existing admission.
9. On admission, the manager commits the normalized display name with the root handle.
10. The fleet stream renders the display name while all actions continue using the immutable root session ID.
11. Every delegated child receives and validates the same repository startup specification before starting Pi.

A rename updates only step 10's manager-owned display projection. It does not alter any bootstrap, process, workspace, or identity value.

## Security and Error Handling

- Names are escaped by `html/template`, length-bounded, stripped of surrounding whitespace, and rejected if they contain control characters.
- Duplicate names are allowed because names are not authorization or routing identities.
- Every route and browser action continues to use validated immutable session IDs.
- Repository choices come only from administrator configuration.
- Browser input never becomes an absolute path, shell fragment, remote URL, or clone command.
- Bootstrap records remain strict, bounded, private inherited data.
- Repository checks use argument arrays and reject symlinks or containment escapes.
- Missing repository failures expose only stable operator copy.
- Logs retain underlying errors for diagnosis without rendering private paths or process details in the browser.
- Rename and launch endpoints retain the existing authenticated-console and same-origin write boundary.
- Invalid launch requests create no process, log, socket, volume, or instance.

## Testing Strategy

Implementation follows test-driven development.

### Manager tests

- optional names normalize and validate before side effects;
- empty and cleared names use the ID fallback;
- duplicate active names are accepted;
- overlong or control-character names are rejected;
- admitted handles retain names while failed launches do not;
- renaming a root updates fleet and session revisions;
- renaming a descendant or missing session fails;
- repository options are sorted, copied, and limited to configured slugs;
- empty repository resolves to `/workspace`;
- unknown repositories fail before spawn artifacts;
- root bootstrap carries the exact resolved repository value.

### Server and browser tests

- the modal renders the optional name input and repository selector;
- configured repository slugs render exactly once in deterministic order;
- no credentials or arbitrary filesystem paths are rendered;
- modal reset clears the name and restores the no-repository default;
- launch JSON includes the exact optional name and repository values;
- rename is available only for root detail views;
- rename Save, Cancel, clear, pending, and failure states behave correctly;
- fleet, detail, and breadcrumbs render escaped display names;
- immutable session IDs remain in route/action attributes and detail metrics;
- authentication and same-origin boundaries cover rename and changed launch requests;
- repository-unavailable startup status renders the specific modal copy;
- other launch failures remain generic and sanitized.

### Bootstrap, supervisor, and provisioning tests

- root and child bootstrap records strictly and boundedly encode the repository start value;
- malformed slugs, unsafe basenames, paths, and unknown fields are rejected;
- children and grandchildren inherit an exactly equal repository selection;
- root and child instance configuration sets the intended Pi working directory;
- empty selection preserves `/workspace`;
- missing, non-directory, symlinked, escaped, nested-worktree, and symlinked-Git cases fail;
- a valid configured checkout starts Pi from its exact top level;
- root startup status reports only allowlisted typed failures;
- child startup uses the existing typed failure channel;
- provisioning cleanup ordering remains intact after validation failure.

### Workspace and image tests

- new and reused seed volumes prepare `/workspace` and `/workspace/repos` with owner/group `kanedias` and mode `0755`;
- empty repository configuration still repairs ownership before warning;
- root provisioning repairs an older cloned volume before Pi starts;
- the launcher safely defaults, validates, changes directory, and rejects missing or unsafe selections;
- child clones retain writable workspace ownership.

### Regression verification

Run focused tests during development, followed by:

```text
go test ./...
go test -race ./internal/manager ./internal/server ./internal/supervisor/... ./internal/workspace
go vet ./...
node --test internal/server/web/*.test.js
git diff --check
```

No destructive live Incus acceptance run is performed without the repository's existing explicit authorization. Read-only inspection may confirm ownership; any live mutation or session launch follows the established acceptance controls.

## Acceptance Criteria

The feature is complete when:

1. New Session accepts an optional display name.
2. A blank name displays the immutable root session ID.
3. Duplicate display names are accepted.
4. An active root can be renamed or cleared from its detail header.
5. Fleet rows, detail headings, and root breadcrumbs update immediately after rename.
6. Immutable session IDs remain visible and continue to drive every route and action.
7. New Session offers only configured repositories plus the `/workspace` default.
8. Selecting a repository starts root Pi at its exact checkout path.
9. Fresh, forked, and nested descendants inherit the same starting repository.
10. A missing or unsafe selected checkout prevents Pi task execution and shows the specific modal error.
11. `kanedias` can create files directly under `/workspace` in newly launched roots.
12. Existing uncorrected seed volumes do not prevent the cloned-root ownership repair.
13. A server restart may lose display names only as explicitly deferred to issue #57.
