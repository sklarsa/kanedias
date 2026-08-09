# Streaming Transcript Interaction Design

## Goal

Keep user-owned transcript interaction stable while a selected Kanedias session continues streaming: an individually opened tool card remains open, and text selected in content that has stopped changing remains selected instead of being destroyed by later Datastar activity patches.

## Problem

The selected-session SSE handler coalesces activity updates every 50 ms and patches the complete `#activity-panel` with Datastar outer morphs. Tool cards are always rendered without `open`, so every patch removes a user-added `open` attribute. The browser also converts server-rendered plain message/tool text into highlighted HTML. On the next patch, the server's plain text differs from that browser-owned rendered subtree, so Datastar rebuilds it even when its underlying activity item is unchanged. Rebuilding those nodes destroys browser selections.

Stopping the server stops these patches, which is why both interactions then persist.

## Scope

This change covers:

- individually toggled tool-card state across activity morphs;
- stable DOM identity for projected activity items;
- stable rendered DOM for completed message text, immutable tool arguments, and completed tool output;
- existing global Ctrl-O tool expansion behavior;
- streaming updates for the currently changing assistant message and running tool output.

It does not pause the SSE stream, persist browser state on the server, preserve a selection inside text that is actively changing, or redesign activity delivery as per-row SSE events.

## Architecture

### Stable activity identity

Every projected activity wrapper receives an HTML `id` derived only from the manager-projected numeric event sequence: `activity-item-<seq>`. Tool cards place it on `<details>`; non-tool entries place it on the outer `.t-turn` element.

The sequence is already the stable identity of a projected item. Assistant text deltas retain the sequence of the first delta, and tool updates retain the tool-start sequence. Numeric sequence values are safe in an HTML ID and require no new trust boundary.

Stable IDs let Datastar match the same activity item directly rather than relying only on sibling position.

### User-owned tool expansion

Each `<details data-tool-card>` includes:

```html
data-preserve-attr="open"
```

Datastar v1.0.2 already treats `data-preserve-attr` as a list of attributes that server morphs must not add, change, or remove. Native click toggles and the existing Ctrl-O controller therefore remain authoritative for `open`. Newly inserted cards still begin collapsed because the server does not emit `open`.

No per-card state map is needed in JavaScript.

### Immutable rendered content

The manager adds a `Complete` flag to projected activity items:

- an assistant `message_update` starts incomplete and becomes complete when its matching `message_end` arrives;
- user messages, model/extension errors, and generic one-shot events are complete when appended;
- a tool item is complete after `tool_execution_end` changes its status to `done`.

The server view exposes this only as presentation booleans; it does not accept browser input.

The template emits Datastar's `data-ignore-morph` on content whose source can no longer change:

- completed message/error/event `.t-body` elements;
- tool argument `<code>` elements, which are immutable after tool start;
- tool result `<code>` elements only after the tool is complete.

The first browser insertion still runs Markdown or syntax highlighting. Later panel patches see `data-ignore-morph` on both the existing and incoming element and leave that rendered subtree untouched. Because the selected DOM nodes remain attached, browser text selections in completed content remain valid.

The currently streaming assistant body and running tool result remain morphable, so live output is not frozen.

## Data Flow

1. Pi events enter `activityProjector`.
2. The projector creates an item with a stable `Seq` and updates its text/tool state in place.
3. Terminal events mark the matching item complete.
4. `newActivityView` maps completion into immutable-content presentation flags.
5. `activity.html` emits stable IDs plus Datastar preservation attributes.
6. Datastar continues morphing `#activity-panel`, but preserves the browser-owned `open` attribute and skips immutable rendered subtrees.
7. The existing mutation observer processes only newly inserted or newly finalized Markdown/code nodes and keeps existing global expansion/scroll behavior.

## Error and Edge Handling

- A malformed or unmatched `message_end` leaves no content incorrectly frozen; only the matching open assistant item can be marked complete.
- Tool arguments remain immutable by the existing projector contract. Tool partial/final output stays morphable until `tool_execution_end`.
- Replay reconstructs the same completion flags deterministically from retained event order.
- Duplicate or missing sequence values are already governed by the supervisor event stream. The design introduces no new fallback identity.
- `data-ignore-morph` is limited to text/code descendants, not the whole card, so tool status classes and summary labels can still update.
- The activity root remains patchable, so warnings, new rows, and scroll behavior continue to work.

## Testing

### Manager projection tests

Add regressions proving:

- a streaming assistant item is incomplete before `message_end`;
- the same item becomes complete after `message_end` without changing its stable sequence;
- tool completion remains represented by status `done`.

### Server rendering tests

Add template assertions proving:

- every activity item renders `id="activity-item-<seq>"`;
- tool cards render `data-preserve-attr="open"` but never server-render `open`;
- completed message bodies render `data-ignore-morph` while streaming bodies do not;
- tool arguments always render `data-ignore-morph`;
- running result output remains morphable and completed result output renders `data-ignore-morph`.

### Existing suites

Run:

```bash
go test ./internal/manager ./internal/server -count=1
node --test internal/server/web/*.test.js
make test
```

### User-flow validation

Against a live streaming session:

1. Open one completed tool card and confirm it stays open while new activity arrives.
2. Select text in a completed message or completed tool result and confirm the selection remains while new activity arrives.
3. Confirm a running tool result and the current assistant response continue updating.
4. Confirm Ctrl-O still expands/collapses all cards and newly inserted cards inherit the global mode.

## Alternatives Rejected

### Capture and restore browser state in JavaScript

A per-card state map plus serialized Selection ranges could reconstruct state after every patch, but range restoration across Markdown/highlighting rewrites is fragile and would duplicate state already supported by Datastar preservation primitives.

### Patch individual activity rows

Per-row append/update SSE events would minimize DOM work but would substantially change handler state, replay behavior, ordering, warning rendering, and tests. It is unnecessary for this bug.

### Freeze the activity panel while text is selected

Ignoring all panel morphs during selection can leave the UI stale and requires an explicit resynchronization path after selection clears. Preserving only immutable nodes avoids that coordination problem.

## Security

All new DOM IDs derive from numeric sequence values. No raw supervisor payload is promoted to HTML, JavaScript, selectors, or trusted markup. Existing `html/template` escaping and manager display bounds remain unchanged. Datastar preservation applies only to browser-rendered descendants whose source projection is complete.
