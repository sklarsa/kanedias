# Composer, Fleet Layout, and Repository Picker Usability Design

**Status:** Approved

**Date:** 2026-08-10

## Purpose

Kanedias currently presents the session composer as a small, fixed, single-line input, fixes the desktop Fleet panel at 340 pixels, and uses a native repository dropdown in the New Session modal. Long prompts are difficult to review, the Fleet can consume more space than an operator wants, and finding one repository in a long dropdown is inefficient.

This design makes the composer readable and multiline, makes the desktop Fleet panel resizable and temporarily collapsible, remembers the operator's desktop layout in the browser, and replaces the repository dropdown with a strict autocomplete over configured repositories.

## Goals

- Make the prompt composer large enough to review multiline text before sending.
- Preserve Enter-to-send while supporting explicit multiline entry with Shift+Enter.
- Grow the composer with its content up to a bounded height.
- Let desktop operators resize the Fleet and main transcript panes.
- Let desktop operators hide and restore the Fleet.
- Remember the desktop Fleet width and collapsed state across reloads in the same browser.
- Preserve the existing mobile Fleet slide-over behavior while accommodating the variable-height composer.
- Filter configured repositories as the operator types.
- Keep `/workspace` as the blank/default repository choice.
- Preserve the server's configured-repository allowlist as the authoritative boundary.
- Provide complete keyboard and assistive-technology behavior for the new controls.

## Non-goals

- Changing prompt transport, message size limits, image attachment semantics, or per-session draft ownership.
- Adding rich-text or Markdown editing to the composer.
- Allowing arbitrary repositories, URLs, branches, commits, or filesystem paths.
- Persisting layout preferences on the server or synchronizing them between browsers.
- Redesigning model and thinking-level selectors in the New Session modal.
- Changing the Fleet tree's contents, filtering semantics, or server-sent update protocol.
- Adding third-party splitter, textarea, or combobox dependencies.

## Chosen Approach

Kanedias will use small dependency-free controllers that match the existing browser architecture:

- the existing composer binding will own textarea autosizing because it already owns draft rendering and session switches;
- a dedicated Fleet layout controller will own resizing, collapsing, viewport adaptation, and browser persistence; and
- a dedicated repository combobox controller will own query, selection, validation, and listbox interaction, while the existing session modal remains responsible for reset, pending state, request construction, and submission.

Native CSS resizing and `<datalist>` were rejected because they provide inconsistent autocomplete presentation, insufficient separator keyboard semantics, and weak control over strict selection. Adding UI libraries was rejected as unnecessary dependency and styling overhead for three bounded controls.

## Composer

### Markup and presentation

The command deck's single-line `.deck-input` becomes a `<textarea>` with two visible rows, a 15-pixel monospace font, and the existing accessible label, placeholder, title, capability state, and class hooks. The textarea starts at two lines and grows as text wraps or explicit newlines are inserted. Growth stops at six lines; additional content scrolls within the textarea.

The app grid changes from a fixed deck row to an automatic final row:

```text
grid-template-rows: var(--topbar-h) minmax(0, 1fr) auto
```

The image attachment tray therefore increases the deck naturally rather than selecting a second fixed height. The main pane retains the remaining bounded viewport height and its existing scrolling.

### Keyboard behavior

The composer keeps the current send convention:

- bare Enter submits the selected draft;
- Shift+Enter inserts a newline;
- composition events never submit;
- existing Ctrl+A, Ctrl+C, Escape, and Ctrl+O behavior remains unchanged.

The key handler prevents the browser's newline only when it accepts a bare Enter submission. Modified Enter combinations other than Shift+Enter receive no new shortcut.

### Autosizing and draft flow

Textarea height is presentation state, not draft state. The existing image/draft controller continues to store the exact text for each session, including newlines.

The composer binding recalculates height after:

- every editable input event;
- selecting or reconciling a session draft;
- programmatic clear or successful send;
- reset after a selected draft changes; and
- a viewport/font metric change that affects wrapping.

Autosizing temporarily resets the textarea height, reads its content height, clamps it between the computed two-line and six-line bounds, and enables vertical overflow only at the upper bound. Switching sessions therefore sizes the composer for the newly visible draft rather than retaining the previous session's height.

Existing attachment paste/drop behavior, busy state, capability gating, focus restoration, and per-session status ownership remain unchanged.

## Desktop Fleet Layout

### Stable shell and separator

A stable separator is added between `#fleet-panel` and `#main-stack` in `index.html`. The desktop app grid becomes:

```text
var(--fleet-width) separator-width minmax(0, 1fr)
```

The separator is outside the Datastar-patched Fleet root, so server-sent Fleet morphs cannot replace the current width, drag state, or separator listeners. The app shell owns a collapsed class that changes the Fleet and separator columns to zero while leaving the main pane available.

The separator has `role="separator"`, `aria-orientation="vertical"`, a focus target, an accessible label, and current/minimum/maximum values. Pointer dragging uses pointer capture so resizing continues reliably until pointer release or cancellation.

### Bounds and defaults

Desktop resizing is available only above the existing 820-pixel mobile breakpoint. The preferred width is clamped to:

- a minimum of 240 pixels;
- a maximum of 560 pixels; and
- no more than half of the current viewport width.

The initial unsaved width remains 340 pixels at viewports of at least 1100 pixels and 300 pixels between 821 and 1099 pixels, matching the current responsive intent. A viewport resize reapplies the clamp without discarding the operator's preferred saved width, so a larger window can restore it later.

Keyboard controls on the focused separator are:

- Left Arrow: decrease preferred width by 16 pixels;
- Right Arrow: increase preferred width by 16 pixels;
- Home: use the current minimum; and
- End: use the current maximum.

### Collapse and restore

The Fleet header gains a bounded **Hide Fleet** control. When collapsed, the top-bar Fleet/menu button is available as **Show Fleet**. On desktop, restoring the Fleet returns it to the clamped preferred width. Controls update `aria-expanded`, `aria-controls`, labels, and visibility to describe the current state.

Actions that explicitly need the Fleet, such as clicking the global question alert, restore it before expanding and scrolling to the target question.

### Persistence

The layout controller stores versioned width and collapsed values in `localStorage`. Storage is browser-local and contains no session, repository, prompt, or server data. Reads accept only finite numeric widths and exact boolean representations; invalid values fall back to defaults. Storage access errors are caught and treated as unavailable.

Dragging or keyboard resizing updates the preferred width. Collapse and restore update the saved collapsed state. The preferred width is retained while collapsed.

### Mobile behavior

At 820 pixels or below, the desktop grid width, separator, and desktop collapsed class do not control the layout. The existing top-bar button opens and closes the Fleet as a full-width slide-over. The sheet covers the variable-height composer while open and ends at the bottom of the dynamic viewport, avoiding any fixed deck-height assumption. It closes on the existing scrim interaction.

Desktop preferences remain stored while mobile is active and are reapplied when the viewport returns to desktop. The mobile sheet starts closed after a page load and is not itself persisted.

## Repository Autocomplete

### Source and trust boundary

The autocomplete contains only the repository options already rendered from the manager's immutable configured repository catalog, plus the blank `/workspace` default. It performs no network search and accepts no browser-invented repository in the launch payload.

The existing manager validation remains authoritative. The browser component improves selection and client feedback but does not weaken or replace server-side allowlist resolution.

### Markup and state

The native repository `<select>` is replaced by an accessible combobox composed of:

- a visible text input with `role="combobox"`, `aria-autocomplete="list"`, `aria-expanded`, and `aria-controls`;
- a popup `role="listbox"` containing the rendered configured choices;
- `role="option"` rows with stable DOM IDs and `aria-selected`; and
- an internal committed value that is separate from the visible query.

A blank visible query commits the empty value and means `/workspace`. Selecting a repository commits the exact canonical slug rendered by the server. Editing a committed query clears that commitment unless the new text exactly matches a configured slug. Case-insensitive exact typing may commit the corresponding canonical slug; the submitted value always uses the server-rendered canonical spelling.

### Filtering and keyboard behavior

Filtering is case-insensitive substring matching over configured slugs. Focusing an empty combobox presents `/workspace` first followed by all configured repositories. A nonempty query presents matching configured slugs. If no option matches, the popup presents a non-selectable no-results status.

Keyboard behavior follows the standard combobox/listbox pattern:

- Down/Up Arrow opens the popup and changes the active option;
- Enter commits the active option;
- Escape closes the popup without launching or closing the modal;
- Home/End move to the first/last visible option while the popup is open; and
- Tab keeps normal focus movement, committing an exact match but not arbitrary text.

Pointer selection commits before blur closes the popup. The active option is exposed through `aria-activedescendant`, and result-count/no-result changes are announced through bounded status text.

### Validation, reset, and submission

Opening or resetting the New Session modal clears both query and committed repository, restoring `/workspace`. The root model retains the modal's current initial focus behavior.

Before launch, the modal asks the combobox for its committed value. Submission is blocked without a request when the visible query is nonblank and no configured repository is committed. The existing modal status region displays:

> Choose a configured repository or clear the field to use /workspace.

The operator's query and every other modal value remain intact after this client-side validation failure. Pending launch disables the combobox with the other controls and closes its popup. Server rejection keeps the exact committed selection visible, matching existing modal failure behavior.

The launch JSON shape does not change: `repository` remains either the empty string or one exact configured slug.

## Component Boundaries

### Existing composer binding (`app.js`)

Owns autosizing integration because it already renders session drafts, observes capability changes, restores focus, and handles programmatic input updates. Pure size calculation is kept independently testable. It does not own Fleet or repository behavior.

### Terminal keyboard helper (`terminal-ui.js`)

Classifies bare Enter as submit and leaves Shift+Enter to the textarea. It remains independent of network and draft state.

### Fleet layout controller (`fleet-layout.js`)

Owns desktop preferred width, effective clamping, drag/keyboard interaction, collapsed state, storage, breakpoint transitions, and ARIA synchronization. It exposes show/hide operations for the global question alert and is independent of Datastar content.

### Repository combobox controller (`repository-combobox.js`)

Owns filtering, active option, canonical commitment, popup state, ARIA state, and validation. It exposes committed value, reset, pending, validate, and destroy operations to the session modal without owning fetch or request semantics.

### Session modal (`session-modal.js`)

Coordinates combobox lifecycle with existing form reset, pending snapshots, strict launch JSON, response handling, and listener cleanup. Model/thinking selector behavior remains unchanged.

## Error Handling and Recovery

- Invalid or unavailable local storage never prevents page startup or interaction.
- Out-of-range saved widths are clamped; nonnumeric values are ignored.
- Pointer cancellation ends resize cleanly without leaving a dragging class or capture.
- A Fleet patch during a drag cannot reset layout because the controller binds to stable shell elements.
- A missing or empty repository catalog still provides `/workspace` and a functional combobox.
- Unmatched repository text cannot reach `fetch` and remains available for correction.
- Server-side unknown-repository errors remain possible under stale or tampered clients and retain existing sanitized modal handling.
- Destroying either new controller removes listeners and prevents stale callbacks from mutating the page, matching existing binding contracts.
- Responsive transitions close stale mobile sheet/scrim state and synchronize desktop controls without losing the preferred desktop values.

## Security and Privacy

- Repository options still originate only from administrator configuration and are rendered with `html/template` escaping.
- No arbitrary query becomes a URL, path, route, selector, trusted HTML, or launch value.
- The submitted repository is always blank or an exact canonical configured slug; the server independently verifies it.
- Layout storage contains only a numeric width and boolean collapsed preference.
- Prompt contents and selected session identifiers are not added to persistent browser storage by this feature.
- Existing same-origin launch, message, and session-action boundaries are unchanged.

## Testing Strategy

Implementation follows test-driven development.

### Composer and keyboard tests

- the template renders a textarea with the intended accessible metadata;
- two-line, intermediate, and over-six-line content clamps correctly;
- vertical overflow begins only at the upper bound;
- wrapping, session switching, clear, and send trigger recalculation;
- multiline drafts remain independent by session;
- bare Enter submits and prevents newline insertion;
- Shift+Enter does not submit and remains available for newline insertion;
- composition and existing terminal shortcuts remain unchanged;
- attachment, busy, focus-restoration, and capability regressions remain covered.

### Fleet layout tests

- defaults match both desktop ranges;
- stored preferred width and collapsed state restore after binding;
- malformed or throwing storage falls back safely;
- pointer movement clamps and persists width;
- pointer release/cancel clears drag state;
- keyboard arrows/Home/End update width and ARIA values;
- collapse, restore, and question-alert reveal synchronize controls and persistence;
- Fleet DOM replacement does not reset layout;
- viewport resizing clamps without overwriting preferred width;
- mobile transitions hide the separator, use the sheet, clear stale scrim state, and restore desktop preferences.

### Repository combobox tests

- empty query means `/workspace` and shows all options on focus;
- substring filtering is case-insensitive and preserves canonical values;
- arrow, Home/End, Enter, Escape, Tab, pointer, and blur behavior is deterministic;
- active descendant, selected option, expanded state, and result announcements synchronize;
- no-result and unmatched query states cannot submit;
- exact typed matches commit canonical slugs;
- reset, cancel, reopen, pending, failed launch, successful launch, and destroy lifecycle remain correct;
- launch JSON includes only blank or configured committed repository values.

### Server rendering and regression verification

Go template tests verify the separator, collapse controls, textarea, combobox roles, escaped repository values, stable option IDs, and `/workspace` default. Existing server and manager launch validation tests remain unchanged and continue proving that unknown repositories fail before spawn side effects.

Final verification runs:

```text
go test ./...
go test -race ./internal/server
go vet ./...
node --test internal/server/web/*.test.js
git diff --check
```

Responsive manual checks cover desktop drag/collapse/reload, keyboard-only operation, mobile Fleet open/close with both plain and image-bearing multiline drafts, and New Session repository filtering.

## Acceptance Criteria

The feature is complete when:

1. The composer starts at two readable lines with a 15-pixel font.
2. It grows through six lines, then scrolls internally without displacing the main pane beyond the viewport.
3. Enter sends, Shift+Enter inserts a newline, and composition cannot submit.
4. Multiline text remains isolated in the correct per-session draft across session switches.
5. Desktop Fleet width can be changed by pointer and keyboard within the specified bounds.
6. The Fleet can be hidden and restored without interrupting live session updates.
7. Preferred Fleet width and collapsed state survive reload in the same browser.
8. Mobile retains a usable Fleet slide-over and does not depend on a fixed composer height.
9. The repository field filters configured slugs as the operator types and supports complete keyboard navigation.
10. Blank repository selection launches at `/workspace`.
11. Unmatched text is rejected before network submission with explicit corrective copy.
12. Launch requests contain only blank or exact configured repository slugs, and server validation remains authoritative.
13. Existing prompt sending, attachment, modal, Fleet streaming, alert navigation, and responsive behavior continue to pass regression tests.
