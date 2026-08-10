# Selected Session Highlight Design

## Goal

Visually highlight the session currently selected in the main detail view within the left fleet list.

“Selected” means the session identified by the browser’s `selectedSessionId` signal. It is distinct from the session lifecycle value `active`.

## Current Behavior and Root Cause

Clicking any root, parent worker, or leaf worker row updates `selectedSessionId` and loads that session’s detail view. The stylesheet already defines the intended selected-row appearance under `.row.sel`, but no fleet row derives the `sel` class from `selectedSessionId`. Consequently, selection state changes without activating the existing highlight.

## Design

Each of the three fleet row variants—root, parent worker, and leaf worker—will declaratively bind its `sel` class to whether its `data-session-id` equals `selectedSessionId`.

The binding will use Datastar’s reactive class attribute on the existing row element. This keeps the signal as the single source of truth, reuses the existing selected-row styling, and automatically restores the correct class when server-rendered fleet fragments are patched into the page.

No backend state, new JavaScript controller, or CSS changes are required.

## Data Flow

1. The user clicks a fleet row.
2. Existing Datastar wiring assigns that row’s session ID to `selectedSessionId` and requests its detail view.
3. Each row’s class binding re-evaluates.
4. Exactly the row whose `data-session-id` matches `selectedSessionId` receives `sel`; all other rows remove it.
5. Existing `.row.sel` CSS provides the visible highlight.

If the selected session disappears from the fleet, no remaining row matches the signal and none is highlighted. This requires no special error handling.

## Testing

Add a focused fleet-template regression test that renders root, parent-worker, and leaf-worker rows and verifies that every session row contains the reactive selected-class binding. The test must fail before the template change and pass after it.

Run the focused Go server/template tests and the repository’s standard test suite to check for regressions.

## Scope

This change only adds selection-class bindings to fleet rows and a regression test. It does not alter lifecycle styling, selection behavior, detail loading, fleet streaming, or backend APIs.
