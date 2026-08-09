# Pi-Like Web Transcript and Terminal Controls — Design

Date: 2026-08-09

## Goal

Make Kanedias's local web console feel like Pi's terminal interface without
turning the proof of concept into a frontend framework migration.

The selected session transcript will render user and assistant Markdown with
syntax-highlighted code. Tool inputs and results will be available in compact,
collapsed cards. The command deck will adopt Pi's core keyboard behavior, and
its action buttons will follow live server state instead of a stale click-time
lifecycle value.

The work will be developed in an isolated worktree, delivered in one pull
request with reviewable commits, and merged only after required GitHub checks
are green.

## Scope

### In scope

- GitHub-Flavored Markdown for both user and assistant transcript messages.
- Pi-compatible handling of headings, emphasis, lists, task lists, tables,
  blockquotes, links, inline code, fenced code, and line breaks.
- Syntax highlighting for fenced code, including language auto-detection when
  no language is declared.
- Safe rendering of raw HTML-like text and untrusted links/images.
- Collapsed tool cards containing a useful summary, formatted arguments, and
  bounded accumulated or final output.
- Per-tool expansion by click and global tool expansion with `Ctrl-O`.
- Pi-like `Ctrl-A`, `Ctrl-C`, `Escape`, and Enter behavior in the command deck.
- Live, authoritative Steer, Interrupt, and Stop capability state.
- A visibly armed Interrupt button while the selected session is running.
- Graceful plain-text fallback if Markdown rendering fails or JavaScript is
  unavailable.
- Automated Go and JavaScript tests for projection, templates, Markdown
  security, key decisions, and control capabilities.

### Out of scope

- A React/Vue/Svelte migration or a JavaScript build pipeline.
- Pixel-identical reproduction of Pi's terminal ANSI theme.
- Rendering arbitrary HTML supplied by a model or tool.
- Executing scripts, loading remote JavaScript/CSS, or embedding arbitrary tool
  media.
- Persisting UI toggle state across page reloads.
- Full durable transcript hydration beyond the existing retained recent
  activity model.
- User-configurable keybinding files in the web UI.

## Chosen Approach

Use Pi's own browser-export rendering stack and configuration as the reference:
vendored `marked` 18.0.5 and Pi's vendored Highlight.js build. This is a closer
behavioral match than introducing a separate Go Markdown implementation, keeps
streamed partial responses responsive, and preserves the existing no-build,
embedded-asset architecture.

The vendored libraries are served only from Kanedias's embedded loopback web
server. No CDN or runtime package installation is used.

## Architecture

The feature is split into four focused units.

### 1. Markdown renderer

A small local browser module owns Markdown configuration and rendering. It:

- configures `marked` with `gfm: true` and `breaks: true`;
- uses Pi's strict strikethrough behavior;
- treats HTML-like input as literal text rather than executable markup;
- allow-lists `http`, `https`, `mailto`, `tel`, and `ftp` URL schemes;
- renders invalid or dangerous links as ordinary text;
- escapes inline code;
- uses Highlight.js for declared languages and auto-detection otherwise; and
- catches parser/highlighter errors and leaves the original escaped text
  visible.

Server templates remain the trust boundary: transcript source is emitted through
`html/template`, so it starts HTML-escaped. A fresh transcript body carries a
`data-markdown` marker and displays its source as plain text. The browser reads
`textContent`, renders it once, and marks the node as complete. This provides a
readable fallback before scripts load and prevents generated HTML from being
mistaken for source on the renderer's own DOM mutation.

A `MutationObserver` processes new transcript nodes after Datastar replaces the
activity panel. Since activity is already coalesced server-side, the renderer
re-renders each new streamed snapshot rather than trying to patch an incomplete
Markdown token stream.

Generated links open safely with `rel="noopener noreferrer"`. Images are
limited to the same safe URL schemes and constrained by transcript CSS; data and
JavaScript URLs are rejected.

### 2. Tool activity projection and cards

The manager's activity projector remains the only path from raw Pi events into
the web view. It will retain display fields from:

- `tool_execution_start.args`;
- `tool_execution_update.partialResult`; and
- `tool_execution_end.result`.

The projection will never pass arbitrary raw HTML. It produces:

- tool ID, name, status, and error state;
- a concise summary for common tools (`bash`, `read`, `write`, `edit`, `grep`,
  `find`, and `ls`);
- indented JSON arguments;
- text content extracted from Pi result content blocks;
- an indented JSON fallback for custom result shapes; and
- an optional language hint inferred from a file path or tool kind.

Arguments and output are each bounded to 64 KiB per activity item, truncated on
valid UTF-8 boundaries with an explicit truncation marker. Binary/image blocks
are summarized rather than embedded. This prevents one retained event from
creating an unbounded DOM subtree while preserving useful proof-of-concept
observability.

Each tool is rendered as a semantic `<details>` card, collapsed by default.
Clicking its summary toggles only that card. `Ctrl-O` expands all collapsed tool
cards or collapses all cards when every card is already open. Datastar
replacements inherit the current global tool mode for the lifetime of the page,
so streaming updates do not unexpectedly collapse an operator's view.

Tool arguments and results use code/preformatted presentation. Known source file
results use the same Highlight.js path as fenced Markdown code; generic JSON is
highlighted as JSON. Tool errors receive a distinct border and status label.

### 3. Command deck keyboard behavior

A pure key-decision helper defines behavior and is called by the delegated
browser handler. It preserves browser-native selection and clipboard behavior.

- `Enter` submits the current deck input and clears it after dispatch.
- `Ctrl-A`, while the deck input is active, moves the caret to the beginning
  instead of selecting the page.
- `Ctrl-C` with selected document or input text is not intercepted, so native
  copy works. With no selection and deck/body focus, it clears the command input
  and emits an `input` event so Datastar state stays synchronized.
- `Escape` clicks Interrupt only when the selected session is currently
  interruptible. It does not send an abort for idle or terminal sessions.
- `Ctrl-O` prevents the browser's Open File shortcut and toggles tool details.

The handler ignores IME composition and does not hijack modified keys inside
question-answer inputs or unrelated controls. Button tooltips and a compact hint
near the deck disclose the available shortcuts.

### 4. Authoritative action capabilities

The current browser logic derives button state only when a fleet row is clicked.
That value becomes stale when Datastar later replaces the row after a lifecycle
transition. This is the cause of the Interrupt button appearing unavailable
while a selected agent is running.

The detail view will project explicit action capabilities from the current
session state:

- `CanSteer`: the selected root is not stale and the node can accept a prompt,
  steer, or follow-up (`ready`, `running`, or `awaiting_handoff`);
- `CanInterrupt`: the selected root is not stale and the node is `running`;
- `CanStop`: the selected route still represents a stoppable/cleanable session;
  stale roots remain stoppable because Stop intentionally evicts an unreachable
  retained root.

A temporarily disconnected event stream does not disable actions while the
manager still owns a current, non-stale route and can reach the supervisor RPC
socket. Capability helpers use the supervisor lifecycle constants and are
table-tested for every known lifecycle.

The detail panel exposes the booleans as inert `data-*` attributes. The browser
observes detail-panel patches and updates the deck from those attributes. A row
click optimistically disables controls while selection changes, but it is no
longer the source of truth.

An enabled Interrupt button gains a clear amber armed state and `Esc` hint. It
returns to disabled styling as soon as the selected session settles or becomes
unreachable. The action endpoint remains server-authorized; browser state is
only a usability layer and never a security boundary.

## Data Flow

### Message update

1. Pi emits text deltas.
2. The supervisor retains raw Pi events within existing event limits.
3. The manager projector coalesces deltas into escaped activity text.
4. The server renders the activity fragment through `html/template`.
5. Datastar replaces `#activity-panel`.
6. The browser observer renders fresh `data-markdown` nodes and highlights code.
7. Existing auto-scroll behavior keeps the newest content visible unless the
   operator has scrolled away from the bottom.

### Tool update

1. Pi emits start, accumulated update, and final result events.
2. The manager correlates them by tool-call ID and updates one bounded activity
   item.
3. The template renders a collapsed tool card.
4. The browser applies highlighting and the current page-level expansion mode.

### Lifecycle update

1. Supervisor lifecycle changes to or from `running`.
2. Manager session state revision changes.
3. The detail stream patches capability attributes.
4. The browser synchronizes disabled/armed button states.
5. Escape and button actions consult the same current DOM capability state.

## Styling and Usability

Markdown will use the existing Astrolabe palette instead of importing Pi's
terminal colors verbatim:

- headings and list bullets use brass accents;
- links use cyan and show a clear hover/focus state;
- inline code uses a subtle sunk background;
- code blocks use a bordered, horizontally scrollable terminal surface;
- quotes use a muted brass rule;
- tables remain readable in the narrow pane and scroll horizontally when needed;
- syntax token colors map onto existing cyan, amber, violet, brass, and muted
  ink variables; and
- text selection remains enabled throughout messages and tool output.

Code and tool blocks will include compact copy controls using the Clipboard API
with a legacy selection fallback. Copy controls must not toggle their parent tool
card and show brief success/failure feedback without alerts.

Keyboard focus indicators remain visible. The feature will not suppress normal
browser copy, link opening, scrolling, or text selection.

## Error Handling and Security

- Raw model HTML is displayed literally; it is never trusted as DOM.
- Dangerous URL schemes and control-character-obfuscated schemes are rejected.
- Markdown parse or highlight errors fall back to escaped plain text for the
  affected body.
- Missing vendor globals leave the transcript readable and log one concise
  browser warning.
- Unsupported tool result blocks are summarized; malformed JSON does not remove
  the tool status card.
- Tool display truncation is explicit and does not alter retained supervisor
  events.
- All action POSTs continue through session authentication and the existing
  same-origin write boundary.
- Disabled controls are not authorization. The manager and supervisor continue
  validating every operation.
- No generated markup uses inline event handlers; interaction remains delegated
  from local application JavaScript.

## Testing Strategy

### Manager tests

- Tool start retains formatted, bounded arguments.
- Partial results replace accumulated display output.
- Final results replace partial output and preserve error state.
- Common tools receive useful summaries and language hints.
- Custom/malformed result shapes degrade to bounded JSON or a safe summary.
- UTF-8 truncation never produces invalid text and visibly marks truncation.

### Server/template tests

- User and assistant messages render escaped source with a Markdown marker.
- Malicious source cannot escape its transcript body before browser rendering.
- Tool cards use semantic collapsed details and escaped argument/result text.
- Detail capability attributes match every lifecycle and stale-state
  combination, and a transient event-stream disconnect does not spuriously
  disable an otherwise reachable route.
- New vendored assets are embedded, served locally with correct content types,
  and referenced without external URLs.

### JavaScript tests

Using Node's built-in test runner and the vendored libraries:

- headings, lists, tables, inline code, and fenced code render correctly;
- declared and auto-detected code blocks receive Highlight.js markup;
- raw HTML is literal;
- JavaScript/data URLs and control-character scheme tricks are rejected;
- safe links include the required relationship attributes;
- key decisions implement Pi's selection-aware `Ctrl-A`, `Ctrl-C`, Escape, and
  `Ctrl-O` behavior; and
- tool toggle decisions are deterministic.

`make test` runs both `go test ./...` and the Node tests. CI continues to call
`make test`, so browser behavior is part of the required PR check.

### Manual acceptance

Against a live local session:

1. Send a prompt containing headings, nested lists, a table, links, inline code,
   and fenced Go/JavaScript/JSON blocks.
2. Confirm streamed Markdown remains readable and settles into highlighted
   output without losing auto-scroll position.
3. Run `read`, `bash`, and one custom tool; inspect individual and global tool
   expansion and copy controls.
4. Select transcript/code text and copy with `Ctrl-C`.
5. Use `Ctrl-A` and `Ctrl-C` in the deck, then send with Enter.
6. Start a turn and confirm Interrupt becomes armed after the live lifecycle
   patch; abort with Escape and confirm it disables after settlement.
7. Confirm Stop remains usable for a retained stale root.
8. Exercise the layout at desktop and mobile widths.

## Delivery

The pull request will contain logically separated commits:

1. the approved design and implementation plan;
2. safe Pi-compatible Markdown and syntax highlighting;
3. bounded expandable tool cards;
4. Pi-like keyboard behavior and authoritative action capability fixes; and
5. any review-driven corrections.

Before opening the PR, run formatting, the hermetic test suite, build, and lint.
After opening it, request an independent code review, address findings, push the
final branch, and wait for required GitHub checks. Merge only when checks are
green and the branch is mergeable.
