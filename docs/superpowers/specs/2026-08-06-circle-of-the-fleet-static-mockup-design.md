# Circle of the Fleet Static Mockup Design

## Goal and Scope

Replace the current placeholder server page with a static, desktop-first “Circle of the Fleet” mockup. The page previews the future Kanedias operator experience for supervising several Pi sessions and nested subagents: parent runs orbit the Maker, nested children appear as moons, paused questions break orbit, and a selected child opens into a detailed transcript and control aperture.

This increment is visual only. Every run, child, question, transcript line, metric, and state is fixed HTML. Controls look like future controls but are disabled and invoke no HTTP route, Datastar action, JavaScript handler, shell, or Pi process. The existing server, health endpoint, one-shot Datastar status endpoint, and vendored Datastar runtime remain intact; the new page does not call the status endpoint.

The approved visual direction is the “Circle of the Fleet” mockup shown during brainstorming. Mobile support is explicitly deferred. The desktop page should remain reachable on a narrow viewport through ordinary horizontal overflow, but this increment does not add a responsive reflow, mobile navigation, touch-specific controls, or mobile acceptance tests.

## Framework Decision

The page will use [Terminal.css](https://github.com/Gioni06/terminal.css) as a lightweight semantic foundation, followed by project-owned `app.css` for the distinctive Kanedias composition.

Three approaches were considered:

1. **Terminal.css plus a custom Kanedias layer — selected.** It is pure CSS, MIT-licensed, approximately 17 KB uncompressed and about 3 KB gzip according to upstream, requires no package manager or build step, includes terminal-oriented typography, cards, alerts, buttons, and forms, and exposes CSS variables. The project-specific layer will substantially reshape it so the result does not resemble a stock Terminal.css page.
2. **Pico CSS plus a custom Kanedias layer.** Pico provides stronger broad form and dialog coverage and good semantic defaults, but its full stylesheet is materially larger and its visual baseline is a modern application rather than an old-machine artifact. Most of its component surface would be unused in this mockup.
3. **Project CSS only.** This would minimize bytes but would not satisfy the request to adopt a framework and would continue reimplementing baseline element, button, form, and terminal primitives that Terminal.css already supplies.

Terminal.css will be vendored unchanged from immutable upstream commit `63551f0de711f2f634a0c2da7bab1d3bae216fef` at `lib/terminal.css`. The expected SHA-256 is:

```text
54382cfc04c064df22f6179453bb3eb85c50fd9cf855f7b57adfbe8c8f75b0f8
```

The source URL is:

```text
https://raw.githubusercontent.com/Gioni06/terminal.css/63551f0de711f2f634a0c2da7bab1d3bae216fef/lib/terminal.css
```

The matching MIT license will be vendored unchanged. `terminal.PROVENANCE` will record the repository, commit, immutable source and license URLs, retrieval date, vendored path, license identifier, digest, and unchanged status. No runtime CDN, font request, package manager, Sass compiler, CSS generator, or build pipeline is added.

## Asset and HTTP Boundaries

All browser resources remain flat files under `internal/server/web` and are embedded by the existing `//go:embed web/*` directive.

New files:

- `internal/server/web/terminal.css`
- `internal/server/web/terminal.LICENSE`
- `internal/server/web/terminal.PROVENANCE`

Modified files:

- `internal/server/web/index.html`
- `internal/server/web/app.css`
- `internal/server/handler.go`
- `internal/server/handler_test.go`

The handler adds one read-only asset route:

| Route | Response |
| --- | --- |
| `GET /assets/terminal.css` | Unchanged embedded Terminal.css with `text/css; charset=utf-8`. |

The existing routes remain unchanged:

- `GET /`
- `GET /healthz`
- `GET /ui/status`
- `GET /assets/app.css`
- `GET /assets/datastar.js`

Unsupported methods continue to return `405`. Terminal.css loads before `app.css`, allowing the project layer to override variables and components without modifying vendored bytes. The existing local Datastar module remains in the page for the server scaffold’s future use, but the mockup contains no Datastar request/action attributes and performs no request after page load.

## Visual System

The page uses a dark, fixed theme derived from the existing Cobalt/Ember palette:

- near-black page and machine backgrounds;
- deep navy panels;
- muted cobalt borders and orbital guides;
- pale blue primary text;
- brighter cobalt for active states;
- muted amber for questions and operator attention;
- restrained ember only for destructive-control previews;
- system monospace fonts only.

Status is never communicated by red/green hue alone. Every state combines text with a stable symbol:

- `● ACTIVE`
- `◇ QUESTION`
- `○ COMPLETE`

The page uses no continuous glitch, flashing, pulsing, rotating, or orbit animation. Scanlines and glow are static and subtle. Focus styles remain visible even though this increment’s action controls are disabled. The color layer targets readable contrast and avoids relying on high-saturation complementary red/green pairs.

## Page Composition

### Command Header

The top header identifies `KANEDIAS // CIRCLE OF THE FLEET`, labels the data as a static demonstration, shows a fake Pi session identifier, and presents a prominent fixed `2 QUESTIONS` alert. The alert is visual content, not a live region update.

### Fleet Orrery

The left side is the primary fleet overview:

- the Maker occupies the center;
- three static orbital rings establish depth;
- four parent-run nodes represent a workflow, workers, and parallel review;
- four child moons demonstrate nested subagent relationships;
- one parent and one child are visibly paused for a question;
- each node includes a human-readable status, current task/tool summary, child count where relevant, and elapsed or completion information;
- a legend explains all symbols.

The orrery is not a graphing library, SVG dependency, canvas, or generated layout. It is semantic HTML positioned by project CSS for this fixed mockup.

### Maker’s Aperture

The right side represents the currently selected nested child. It contains:

- a breadcrumb-like parent/child identity;
- static tabs for question, transcript, tools, and artifacts, with only the question tab presented as selected;
- the complete fixed question and three disabled answer affordances;
- a recursive text tree showing parent, child, and grandchild relationships;
- a short mock execution transcript;
- token, tool, turn, and elapsed metrics.

The aperture is intentionally detailed enough to validate future information hierarchy without claiming that any data is available yet.

### Command Deck

The bottom deck previews the future operator entry point and controls:

- a non-editable prompt-like text surface;
- disabled `Steer`, `Interrupt`, `Stop run`, and `Forge orbit` controls;
- an explicit `Static mockup` label.

No form submission, content editing, keyboard shortcut, or shell process exists in this increment. Native `disabled` attributes are used where possible so inert controls are not misleadingly operable.

## Future Subagent Integration Seam

The future live implementation is expected to use a project-owned Pi extension bridge plus server-side lifecycle-artifact reconciliation:

- the Pi extension will consume pi-subagents’ same-process RPC and owning-session events;
- the Go server will reconcile `status.json`, `events.jsonl`, transcript/log paths, and nested lifecycle records for replay;
- full supervisor questions will cross the trusted bridge because the lifecycle projection alone does not reliably preserve question text;
- steering, interrupt, resume, stop, spawn, and supervisor replies will route through the owning Pi extension rather than direct filesystem writes;
- the browser will receive a Kanedias-owned, versioned and redacted view model.

This architecture is context for stable mockup boundaries only. No extension, bridge, watcher, DTO, RPC call, filesystem watcher, persistence, authentication, or live Datastar stream is implemented now.

The mockup gives its major regions stable IDs suitable for later replacement:

- `fleet-orbit`
- `maker-aperture`
- `question-alert`
- `command-deck`

These IDs are presentation seams, not an API contract.

## Error Handling and Accessibility

The server’s existing construction-time template parsing, embedded-asset error handling, generic client errors, panic recovery, and structured request logging remain unchanged.

The static page uses semantic landmarks and headings. The visual fleet includes text alternatives for every symbolic state. The fixed question is ordinary readable content. Disabled controls are native buttons and do not receive fake links or click handlers. Decorative rings, scanlines, and marks are hidden from assistive technology where represented by elements. There is no autoplay, sound, animation, focus trap, dialog, tooltip-only content, or timed interaction.

## Testing and Verification

Handler tests will be updated before implementation and will cover:

- `GET /assets/terminal.css` returning nonempty embedded CSS with the explicit content type;
- unsupported methods returning `405` for the new asset route;
- all six routes remaining available after changing the process working directory to an empty temporary directory;
- the page loading Terminal.css before `app.css`, followed by the existing local Datastar module;
- no external runtime assets, URLs, imports, fonts, package-manager paths, or CDN references in project-authored HTML/CSS;
- presence of the four stable region IDs and representative parent, nested child, paused-question, transcript, metrics, and command-deck content;
- exact state labels/symbols so color is not the only status signal;
- controls being disabled and the page containing no form, Datastar action/request attribute, automatic request mechanism, inline fetch, or shell/API endpoint binding;
- the obsolete dashboard/session placeholder text and refresh-status control being absent;
- the vendored stylesheet matching the digest in `terminal.PROVENANCE`.

Repository verification remains:

```text
go test ./internal/server -count=1
go test ./... -count=1
go test -race ./internal/server ./cmd -count=1
go vet ./...
go build ./...
git diff --check
```

A local smoke test will build the binary, start it on loopback, curl `/`, `/healthz`, `/ui/status`, and all three browser assets, confirm the static fleet markers, verify `404`/`405` behavior, and stop the process with `SIGTERM`.

## Explicit Non-Goals

This increment does not add:

- the Kanedias Pi extension bridge;
- pi-subagents RPC, lifecycle watching, transcript tailing, or supervisor coordination;
- live sessions, nested runs, progress, questions, alerts, replay, steering, interruption, resume, stop, or spawn;
- a shell, PTY, terminal emulator, command execution, or editable prompt;
- new Datastar endpoints, live SSE, polling, WebSockets, or browser-side application logic;
- authentication, authorization, remote serving, persistence, or a database;
- mobile layout, responsive reflow, mobile navigation, or touch-specific behavior;
- external fonts, images, icons, scripts, stylesheets, analytics, or telemetry;
- npm, a lockfile, Sass, Tailwind, Pico, CSS generation, or any frontend build step;
- changes to CLI behavior, listen security, HTTP lifecycle, existing Datastar dependencies, or Incus behavior.
