# Manual Tool Expansion During Streaming Design

## Goal

Keep a manually toggled tool-result card in its chosen open or closed state while the selected agent continues streaming text, including after Ctrl-O has established a global tool-card mode.

## Root Cause

Datastar correctly preserves each `<details>` element's browser-owned `open` attribute during activity-panel morphs. The separate JavaScript tool-expansion controller causes the failure.

After Ctrl-O runs, the controller stores a global expansion mode. The activity panel's MutationObserver calls the controller's `refresh` method after every streamed mutation. `refresh` currently reapplies the stored mode to every existing tool card, overwriting a later manual click. For a stored collapsed mode, a manually opened card therefore closes on the next generated token.

## Behavior

- Ctrl-O continues to expand or collapse all tool cards currently in the transcript.
- Ctrl-O continues to establish the initial mode for tool cards inserted later.
- A manual click on an existing card overrides Ctrl-O for that card.
- Subsequent streamed activity does not overwrite that manual choice.
- A later Ctrl-O command may once again change every current card.
- With no Ctrl-O mode established, cards keep their existing server/browser defaults.

## Architecture

The tool-expansion controller will track cards it has already encountered in a `WeakSet`.

- `toggle(root)` computes the next global mode, applies it to every current card, and records those cards as seen.
- `refresh(root)` applies the stored global mode only to cards not yet in the seen set, then records them.
- Existing seen cards are left untouched during refresh, making their native `open` state browser-owned between explicit Ctrl-O commands.

A `WeakSet` avoids retaining detached cards after Datastar removes them. The change remains isolated to `internal/server/web/terminal-ui.js`; the activity template and SSE protocol do not change.

## Data Flow

1. Ctrl-O calls `toggle`, which changes all current cards and stores the resulting global mode.
2. The user manually opens or closes an individual card through native `<details>` behavior.
3. Streamed activity mutates the activity panel.
4. The MutationObserver calls `refresh`.
5. `refresh` skips cards already seen, preserving manual state, and applies the global mode only to newly inserted cards.
6. A future Ctrl-O command intentionally applies a new mode to all current cards.

## Testing

Browser-unit regressions will prove:

- a manually opened card stays open when the stored global mode is collapsed and `refresh` runs;
- a manually closed card stays closed when the stored global mode is expanded and `refresh` runs;
- a newly inserted card inherits the stored global mode;
- a later Ctrl-O command still changes all current cards.

Run the focused Node suite and the repository test target after implementation.

## Alternatives Rejected

### Per-card override map

Listening for every native toggle and maintaining explicit exceptions would work, but duplicates state already represented by each `<details>.open` property and requires more lifecycle handling.

### Clear global mode after a manual toggle

This would preserve the clicked card but prevent future cards from inheriting the user's last Ctrl-O choice.

### Change Datastar preservation

Datastar already preserves `open` correctly. Modifying the template, SSE patches, or vendored Datastar would address the wrong state owner.

## Security and Error Handling

No untrusted data, network protocol, or HTML rendering changes are introduced. `WeakSet` membership is limited to DOM elements returned by the existing `[data-tool-card]` query. If there is no global mode, refresh remains a no-op for card state.
