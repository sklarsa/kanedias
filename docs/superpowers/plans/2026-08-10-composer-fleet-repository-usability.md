# Composer, Fleet Layout, and Repository Picker Usability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver a readable auto-growing multiline composer, a persistent resizable/collapsible desktop Fleet pane, and a strict keyboard-accessible repository autocomplete.

**Architecture:** Keep session draft ownership in the existing composer binding, but add a pure exported autosize helper. Add two focused dependency-free UMD controllers: one for stable app-shell Fleet layout and one for repository combobox state; integrate them through the existing `app.js` and `session-modal.js` lifecycle boundaries. Continue to render all trusted repository choices on the server and keep manager-side allowlist validation unchanged.

**Tech Stack:** Go 1.x server with `html/template` and embedded static assets; dependency-free browser JavaScript using the existing UMD/CommonJS pattern; CSS Grid; Node's built-in test runner.

## Global Constraints

- Perform every command and edit in `/home/steven/source/github/kanedias/.worktrees/ux-usability` on branch `feat/ux-usability`; do not modify the main worktree.
- Add no third-party runtime or test dependencies and no remote assets.
- The composer uses a 15-pixel monospace font, starts at two lines, grows through six lines, and scrolls internally beyond six lines.
- Bare Enter submits, Shift+Enter inserts a newline, and IME composition never submits.
- Preserve existing per-session drafts, image attachments, status ownership, capability gating, focus restoration, and Ctrl+A/C/O plus Escape behavior.
- Desktop Fleet width is bounded to 240–560 pixels and at most 50 percent of the viewport; defaults remain 340 pixels at widths of at least 1100 pixels and 300 pixels at 821–1099 pixels.
- The existing 820-pixel mobile breakpoint remains authoritative; the mobile sheet is not persisted.
- Persist only the preferred desktop Fleet width and desktop collapsed boolean in browser-local storage.
- Repository input accepts only `/workspace` (empty value) or an exact server-rendered configured repository slug.
- Preserve server-side repository allowlist validation as the authoritative security boundary.
- Keep all user-controlled content escaped; never promote repository query text into trusted HTML, URLs, paths, routes, or selectors.
- Follow test-driven development and commit after each independently testable task.

---

## File Structure

### New files

- `internal/server/web/fleet-layout.js` — pure Fleet width/default helpers plus the desktop/mobile layout controller.
- `internal/server/web/fleet-layout.test.js` — width, persistence, pointer, keyboard, collapse, patch, and breakpoint tests.
- `internal/server/web/repository-combobox.js` — repository filtering, active-option, canonical commitment, validation, ARIA, and lifecycle controller.
- `internal/server/web/repository-combobox.test.js` — repository query, keyboard, pointer, reset, pending, and destroy tests.

### Modified files

- `internal/server/web/index.html` — textarea, stable separator, and ordered local controller scripts.
- `internal/server/web/fleet.html` — delegated desktop Hide Fleet control inside the patched Fleet header.
- `internal/server/web/session-modal.html` — repository combobox/listbox markup rendered from configured options.
- `internal/server/web/app.css` — automatic deck row, textarea sizing, three-column desktop shell, separator/collapsed state, combobox, and mobile overrides.
- `internal/server/web/terminal-ui.js` — explicit bare-Enter versus Shift+Enter decision.
- `internal/server/web/terminal-ui.test.js` — keyboard regressions.
- `internal/server/web/app.js` — textarea autosizing integration and Fleet controller use by global question navigation.
- `internal/server/web/app.test.js` — autosize and multiline session-draft regressions.
- `internal/server/web/session-modal.js` — repository controller lifecycle, pre-submit validation, pending/reset behavior, and committed request value.
- `internal/server/web/session-modal.test.js` — modal integration around committed/unmatched repository values.
- `internal/server/handler.go` — embedded routes for the two new local JavaScript assets.
- `internal/server/handler_test.go` — asset, script-order, template, and stylesheet assertions.

---

### Task 1: Multiline Auto-Growing Composer

**Files:**
- Modify: `internal/server/web/index.html:40-69`
- Modify: `internal/server/web/app.css:20-35, 60-80, 743-855, 1020-1065`
- Modify: `internal/server/web/terminal-ui.js:9-27`
- Modify: `internal/server/web/terminal-ui.test.js:7-34`
- Modify: `internal/server/web/app.js:10-279`
- Modify: `internal/server/web/app.test.js:10-517`
- Modify: `internal/server/handler_test.go:430-610, 1690-1750`

**Interfaces:**
- Consumes: Existing `KanediasImageAttachments` controller snapshots and `KanediasTerminalUI.performAction` submission callback.
- Produces: `KanediasComposerUI.autoSizeComposer(input, windowObject): {height:number, minHeight:number, maxHeight:number, overflowing:boolean}`; updated `keyAction(event, context)` where bare Enter in the deck returns `"submit"` and Shift+Enter returns `null`.

- [ ] **Step 1: Add failing keyboard and shell-rendering tests**

Extend `terminal-ui.test.js` with explicit newline and composition cases:

```js
test("bare Enter submits while Shift-Enter remains a textarea newline", () => {
  const deck = {target:"deck", hasSelection:false, canInterrupt:true};
  assert.equal(ui.keyAction(event("Enter"), deck), "submit");
  assert.equal(ui.keyAction(event("Enter", {shiftKey:true}), deck), null);
  assert.equal(ui.keyAction(event("Enter", {isComposing:true}), deck), null);
  assert.equal(ui.keyAction(event("Enter"), {...deck, target:"body"}), null);
});
```

Update `TestInitialPageContainsAstrolabeConsole` and `TestAstrolabeConsoleIsInteractive` in `handler_test.go` to require a textarea rather than a text input:

```go
for _, want := range []string{
    `<textarea class="deck-input" rows="2"`,
    `aria-keyshortcuts="Control+A Control+C Enter Shift+Enter"`,
    `Enter: steer · Shift+Enter: newline`,
} {
    if !strings.Contains(body, want) {
        t.Errorf("composer shell does not contain %q", want)
    }
}
if strings.Contains(body, `<input class="deck-input" type="text"`) {
    t.Error("composer still renders the retired single-line input")
}
```

- [ ] **Step 2: Run the focused tests and confirm they fail**

Run:

```bash
node --test internal/server/web/terminal-ui.test.js
go test ./internal/server -run 'TestInitialPageContainsAstrolabeConsole|TestAstrolabeConsoleIsInteractive' -count=1
```

Expected: the Node assertion for Shift+Enter and the Go textarea/template assertions fail against the single-line implementation.

- [ ] **Step 3: Implement the textarea markup and keyboard decision**

Replace the deck input in `index.html` with:

```html
<textarea class="deck-input" rows="2" placeholder="steer — type a directive…" aria-label="Message to selected session"
  aria-keyshortcuts="Control+A Control+C Enter Shift+Enter"
  title="Ctrl+A: select all · Ctrl+C: clear or copy · Enter: steer · Shift+Enter: newline"></textarea>
```

Change the shortcut hint to include `↵ send · ⇧↵ newline`. In `terminal-ui.js`, evaluate Enter before the general modifier guard but after the IME/Alt/Meta checks:

```js
if (!event || event.isComposing || event.altKey || event.metaKey) return null;
if (event.key === "Enter" && context.target === "deck") {
  return !event.ctrlKey && !event.shiftKey ? "submit" : null;
}
if (event.shiftKey) return null;
```

Keep every existing Ctrl and Escape decision unchanged.

- [ ] **Step 4: Add failing autosize and multiline draft tests**

Extend the fake element/window support in `app.test.js` with `style`, `scrollHeight`, and `getComputedStyle`, then add:

```js
test("autoSizeComposer clamps a textarea from two through six lines", () => {
  const input = new FakeElement("textarea");
  input.style = {};
  input.scrollHeight = 30;
  const windowObject = {
    getComputedStyle: () => ({
      lineHeight: "20px", paddingTop: "5px", paddingBottom: "5px",
      borderTopWidth: "1px", borderBottomWidth: "1px"
    })
  };

  assert.deepEqual(app.autoSizeComposer(input, windowObject), {
    height: 52, minHeight: 52, maxHeight: 132, overflowing: false
  });
  input.scrollHeight = 220;
  assert.deepEqual(app.autoSizeComposer(input, windowObject), {
    height: 132, minHeight: 52, maxHeight: 132, overflowing: true
  });
  assert.equal(input.style.height, "132px");
  assert.equal(input.style.overflowY, "auto");
});
```

Add a composer-binding regression proving a newline-bearing draft resizes independently when switching A → B → A:

```js
test("multiline drafts resize independently when the selected session changes", () => {
  const f = fixture();
  const input = f.document.getElementById("deck-input");
  f.select("A");
  input.value = "alpha\nsecond line\nthird line";
  input.scrollHeight = 90;
  input.dispatchEvent(browserEvent("input"));
  const alphaHeight = input.style.height;

  f.select("B");
  assert.equal(input.value, "");
  assert.notEqual(input.style.height, alphaHeight);
  f.select("A");
  assert.equal(input.value, "alpha\nsecond line\nthird line");
  assert.equal(input.style.height, alphaHeight);
});
```

- [ ] **Step 5: Run the autosize tests and confirm they fail**

Run:

```bash
node --test --test-name-pattern='autoSizeComposer|multiline drafts' internal/server/web/app.test.js
```

Expected: FAIL because `autoSizeComposer` is not exported and bound draft rendering does not recalculate height.

- [ ] **Step 6: Implement and bind `autoSizeComposer`**

Add a pure helper near the top of `app.js`:

```js
function numericStyle(style, name) {
  var value = parseFloat(style && style[name]);
  return Number.isFinite(value) ? value : 0;
}

function autoSizeComposer(input, windowObject) {
  var style = windowObject.getComputedStyle(input);
  var lineHeight = numericStyle(style, "lineHeight") || 21.75;
  var chrome = numericStyle(style, "paddingTop") + numericStyle(style, "paddingBottom") +
    numericStyle(style, "borderTopWidth") + numericStyle(style, "borderBottomWidth");
  var minHeight = Math.ceil(lineHeight * 2 + chrome);
  var maxHeight = Math.ceil(lineHeight * 6 + chrome);
  input.style.height = "auto";
  var desired = Math.ceil(input.scrollHeight + numericStyle(style, "borderTopWidth") + numericStyle(style, "borderBottomWidth"));
  var height = Math.max(minHeight, Math.min(maxHeight, desired));
  var overflowing = desired > maxHeight;
  input.style.height = height + "px";
  input.style.overflowY = overflowing ? "auto" : "hidden";
  return {height: height, minHeight: minHeight, maxHeight: maxHeight, overflowing: overflowing};
}
```

Call it after `renderDraft` assigns `input.value`, after every editable `input` event, and from a removable window `resize` listener. When `documentObject.fonts.ready` is available, schedule one guarded recalculation after it resolves; the binding's existing destroyed flag must prevent a late font-ready callback from mutating the page. Export it alongside `bindComposer`:

```js
return {autoSizeComposer: autoSizeComposer, bindComposer: bindComposer};
```

Use the existing `listen` cleanup mechanism so `destroy()` removes the resize listener.

- [ ] **Step 7: Make the deck row automatic and style the textarea**

In `app.css`:

```css
.app{
  grid-template-rows:var(--topbar-h) minmax(0,1fr) auto;
}
.deck-input{
  min-height:0;
  resize:none;
  overflow-y:hidden;
  font-size:15px;
  line-height:1.45;
  white-space:pre-wrap;
}
```

Remove `--deck-h`, the fixed `.app.has-image-draft` deck height, and any mobile assumptions that size only the textarea; retain the existing deck/attachment spacing and horizontal button behavior. Add stylesheet assertions for `grid-template-rows`, `resize:none`, `font-size:15px`, and `line-height:1.45`.

- [ ] **Step 8: Run the complete composer-focused test set**

Run:

```bash
node --test internal/server/web/terminal-ui.test.js internal/server/web/app.test.js
go test ./internal/server -run 'TestInitialPageContainsAstrolabeConsole|TestAstrolabeConsoleIsInteractive|TestProjectStylesDefineAstrolabeVisualSystem' -count=1
git diff --check
```

Expected: PASS.

- [ ] **Step 9: Commit the composer deliverable**

```bash
git add internal/server/web/index.html internal/server/web/app.css \
  internal/server/web/terminal-ui.js internal/server/web/terminal-ui.test.js \
  internal/server/web/app.js internal/server/web/app.test.js internal/server/handler_test.go
git commit -m "feat(ui): add multiline auto-growing composer"
```

---

### Task 2: Persistent Resizable and Collapsible Fleet

**Files:**
- Create: `internal/server/web/fleet-layout.js`
- Create: `internal/server/web/fleet-layout.test.js`
- Modify: `internal/server/web/index.html:15-38, 77-85`
- Modify: `internal/server/web/fleet.html:1-14`
- Modify: `internal/server/web/app.css:20-80, 148-260, 1010-1080`
- Modify: `internal/server/web/app.js:280-600`
- Modify: `internal/server/handler.go:96-128`
- Modify: `internal/server/handler_test.go:180-330, 430-610, 782-815, 1580-1750`

**Interfaces:**
- Consumes: Stable `.app`, `#fleet-panel`, `#fleet-resizer`, `#main-stack`, `#menuBtn`, and `#scrim` elements; patched `[data-fleet-collapse]`; browser `matchMedia`, `localStorage`, `MutationObserver`, and pointer events.
- Produces: `KanediasFleetLayout.defaultWidth(viewportWidth): number`, `clampWidth(preferredWidth, viewportWidth): number`, and `bind(documentObject, windowObject, storageObject?): {show():void, hide():void, toggle():void, sync():void, state():object, destroy():void}`.

- [ ] **Step 1: Create failing pure-state and persistence tests**

Create `fleet-layout.test.js` with a minimal fake DOM/storage and these assertions:

```js
test("desktop defaults and clamps match the approved bounds", () => {
  assert.equal(layout.defaultWidth(1400), 340);
  assert.equal(layout.defaultWidth(900), 300);
  assert.equal(layout.clampWidth(100, 1400), 240);
  assert.equal(layout.clampWidth(900, 1400), 560);
  assert.equal(layout.clampWidth(560, 900), 450);
});

test("saved preferred width survives a narrow viewport clamp", () => {
  const storage = fakeStorage({
    "kanedias.fleet.width.v1": "540",
    "kanedias.fleet.collapsed.v1": "false"
  });
  const f = fixture({innerWidth: 900, storage});
  const controller = layout.bind(f.document, f.window, storage);
  assert.equal(controller.state().preferredWidth, 540);
  assert.equal(controller.state().effectiveWidth, 450);
  f.window.innerWidth = 1400;
  f.window.dispatch("resize");
  assert.equal(controller.state().effectiveWidth, 540);
});

test("invalid or throwing storage falls back without aborting bind", () => {
  const f = fixture({innerWidth: 1400, storage: throwingStorage()});
  assert.doesNotThrow(() => layout.bind(f.document, f.window, f.storage));
  assert.equal(layout.bind(f.document, f.window, fakeStorage({
    "kanedias.fleet.width.v1": "not-a-number"
  })).state().preferredWidth, 340);
});
```

- [ ] **Step 2: Run the Fleet test and confirm it fails**

Run:

```bash
node --test internal/server/web/fleet-layout.test.js
```

Expected: FAIL because `fleet-layout.js` does not exist.

- [ ] **Step 3: Implement width helpers and controller state**

Create `fleet-layout.js` in the repository's existing UMD style. Use exact constants and storage parsing:

```js
var MOBILE_MAX = 820;
var MIN_WIDTH = 240;
var MAX_WIDTH = 560;
var WIDTH_KEY = "kanedias.fleet.width.v1";
var COLLAPSED_KEY = "kanedias.fleet.collapsed.v1";

function defaultWidth(viewportWidth) {
  return viewportWidth >= 1100 ? 340 : 300;
}

function clampWidth(preferredWidth, viewportWidth) {
  var viewportMaximum = Math.floor(viewportWidth * 0.5);
  return Math.max(MIN_WIDTH, Math.min(MAX_WIDTH, viewportMaximum, preferredWidth));
}
```

`bind` must keep `preferredWidth` separate from `effectiveWidth`, obtain `windowObject.localStorage` inside `try/catch` when no test storage is injected, catch every storage read/write, set `--fleet-width` on `.app`, and toggle `.fleet-collapsed` only when `innerWidth > 820`.

- [ ] **Step 4: Add failing interaction and lifecycle tests**

Add tests covering pointer capture, keyboard resizing, delegated collapse after a Fleet patch, question-driven `show`, mobile sheet state, and cleanup:

```js
test("pointer and keyboard resizing clamp, persist, and synchronize ARIA", () => {
  const f = fixture({innerWidth: 1400});
  const controller = layout.bind(f.document, f.window, f.storage);
  f.resizer.dispatch("pointerdown", {pointerId: 7, clientX: 340});
  f.window.dispatch("pointermove", {pointerId: 7, clientX: 460});
  f.window.dispatch("pointerup", {pointerId: 7, clientX: 460});
  assert.equal(controller.state().effectiveWidth, 460);
  assert.equal(f.resizer.getAttribute("aria-valuenow"), "460");
  assert.equal(f.storage.getItem("kanedias.fleet.width.v1"), "460");
  f.resizer.dispatch("keydown", key("Home"));
  assert.equal(controller.state().effectiveWidth, 240);
  f.resizer.dispatch("keydown", key("End"));
  assert.equal(controller.state().effectiveWidth, 560);
});

test("patched collapse controls hide and restore through the stable top bar", () => {
  const f = fixture({innerWidth: 1400});
  const controller = layout.bind(f.document, f.window, f.storage);
  f.installCollapseButton().click();
  assert.equal(controller.state().collapsed, true);
  assert.equal(f.app.classList.contains("fleet-collapsed"), true);
  assert.equal(f.storage.getItem("kanedias.fleet.collapsed.v1"), "true");
  f.menu.click();
  assert.equal(controller.state().collapsed, false);
});

test("mobile uses a non-persistent sheet and restores desktop preferences", () => {
  const f = fixture({innerWidth: 700});
  const controller = layout.bind(f.document, f.window, f.storage);
  f.menu.click();
  assert.equal(f.sidebar.classList.contains("open"), true);
  assert.equal(f.scrim.classList.contains("show"), true);
  assert.equal(f.storage.getItem("kanedias.fleet.collapsed.v1"), null);
  f.window.innerWidth = 1400;
  f.window.dispatch("resize");
  assert.equal(f.sidebar.classList.contains("open"), false);
  assert.equal(f.scrim.classList.contains("show"), false);
  controller.destroy();
  assert.equal(f.listenerCount(), 0);
});
```

- [ ] **Step 5: Implement interaction, synchronization, and cleanup**

Complete `bind` with:

- delegated document clicks for `#menuBtn`, `#scrim`, and `[data-fleet-collapse]`;
- pointerdown on the stable separator and window pointermove/up/cancel with pointer-ID checks;
- 16-pixel ArrowLeft/ArrowRight steps plus Home/End;
- `show`, `hide`, and `toggle` that branch at `innerWidth <= 820`;
- a Fleet-panel `MutationObserver` that reapplies labels/ARIA to patched controls;
- viewport resize that closes stale mobile sheet/scrim and reapplies the desktop effective width;
- `aria-valuemin`, `aria-valuemax`, and `aria-valuenow` based on the current viewport;
- a removable `beforeunload` listener that invokes the idempotent `destroy`; and
- idempotent `destroy` that releases capture, disconnects the observer, and removes all listeners.

Export constants only through behavior, not as mutable public state:

```js
return {
  defaultWidth: defaultWidth,
  clampWidth: clampWidth,
  bind: bind
};
```

- [ ] **Step 6: Add failing shell, asset, and style assertions**

Before changing production templates/routes, update `handler_test.go` to require:

```go
for _, want := range []string{
    `id="fleet-resizer"`, `role="separator"`, `aria-orientation="vertical"`,
    `aria-controls="fleet-panel main-stack"`, `tabindex="0"`,
    `data-fleet-collapse`, `aria-label="Hide Fleet"`,
    `src="/assets/fleet-layout.js"`,
} {
    if !strings.Contains(body, want) { t.Errorf("Fleet shell missing %q", want) }
}
```

Add `/assets/fleet-layout.js` to asset content-type, method-rejection, authentication/CSP, and nonempty-asset tables. Increase the local script count from 8 to 9 and assert `fleet-layout.js` loads before `app.js` with no inline body.

Extend style checks with exact selectors/tokens:

```go
for _, want := range []string{
    `--fleet-width:`, `grid-template-columns:var(--fleet-width)`,
    `#fleet-resizer{`, `.app.fleet-collapsed`,
    `.fleet-collapse`, `cursor:col-resize`,
} {
    if !strings.Contains(styles, want) { t.Errorf("Fleet style missing %q", want) }
}
```

- [ ] **Step 7: Integrate the Fleet controller into the shell**

Add the stable separator between `#fleet-panel` and `#main-stack`:

```html
<div id="fleet-panel"></div>
<div id="fleet-resizer" role="separator" aria-label="Resize Fleet panel"
  aria-orientation="vertical" aria-controls="fleet-panel main-stack"
  aria-valuemin="240" aria-valuemax="560" aria-valuenow="340" tabindex="0"></div>
<div id="main-stack">…</div>
```

Add the delegated header button to `fleet.html`:

```html
<button type="button" class="fleet-collapse" data-fleet-collapse
  aria-label="Hide Fleet" aria-controls="fleet-panel">‹</button>
```

Load `/assets/fleet-layout.js` after `terminal-ui.js` and before `app.js`. Register and serve it in `handler.go` as `text/javascript; charset=utf-8`.

Update the desktop app grid to three columns/areas and style the separator, collapse state, top-bar restore button, and focus/drag states. At the mobile breakpoint, restore a one-column grid, hide the separator and desktop collapse button, clear collapsed visibility overrides, and set the fixed Fleet sheet to `bottom:0` so it overlays the dynamic composer.

In full-page startup in `app.js`, replace the old mobile sheet helpers/listener with:

```js
var fleetLayout = window.KanediasFleetLayout.bind(document, window);
```

In the global question alert handler, call `fleetLayout.show()` before expanding/scrolling to the first question. Do not duplicate Fleet state in `app.js`.

- [ ] **Step 8: Run focused Fleet tests**

Run:

```bash
node --test internal/server/web/fleet-layout.test.js internal/server/web/app.test.js
go test ./internal/server -run 'TestHandler|TestInitialPage|TestAstrolabe|TestProjectStyles|TestTemplatesDefineStableRoots' -count=1
git diff --check
```

Expected: PASS.

- [ ] **Step 9: Commit the Fleet deliverable**

```bash
git add internal/server/web/fleet-layout.js internal/server/web/fleet-layout.test.js \
  internal/server/web/index.html internal/server/web/fleet.html internal/server/web/app.css \
  internal/server/web/app.js internal/server/handler.go internal/server/handler_test.go
git commit -m "feat(ui): resize and collapse the Fleet panel"
```

---

### Task 3: Strict Repository Autocomplete

**Files:**
- Create: `internal/server/web/repository-combobox.js`
- Create: `internal/server/web/repository-combobox.test.js`
- Modify: `internal/server/web/session-modal.html:10-20`
- Modify: `internal/server/web/session-modal.js:1-363`
- Modify: `internal/server/web/session-modal.test.js:1-606`
- Modify: `internal/server/web/app.css:875-1015, 1060-1105`
- Modify: `internal/server/web/index.html:77-86`
- Modify: `internal/server/handler.go:96-130`
- Modify: `internal/server/handler_test.go:180-410, 500-610, 1580-1750`

**Interfaces:**
- Consumes: Server-rendered `[data-repository-option]` elements whose `data-value` is either empty or an exact configured canonical slug.
- Produces: `KanediasRepositoryCombobox.bind(root, documentObject): {value():string, query():string, validate():{valid:boolean,message:string}, reset():void, setPending(boolean):void, destroy():void}`; `KanediasSessionModal.bind(documentObject, fetchFunction)` continues returning the existing modal controller API.

- [ ] **Step 1: Create failing combobox state and keyboard tests**

Create `repository-combobox.test.js` with fake input/listbox/options and assert canonical filtering/commitment:

```js
test("blank query commits workspace and focus shows every option", () => {
  const f = fixture(["", "one/alpha", "two/beta"]);
  const controller = combo.bind(f.root, f.document);
  f.query.focus();
  assert.equal(controller.value(), "");
  assert.deepEqual(f.visibleValues(), ["", "one/alpha", "two/beta"]);
  assert.equal(f.query.getAttribute("aria-expanded"), "true");
});

test("filtering is case-insensitive but commits canonical server spelling", () => {
  const f = fixture(["", "Owner/Alpha", "two/beta"]);
  const controller = combo.bind(f.root, f.document);
  f.query.input("owner/a");
  assert.deepEqual(f.visibleValues(), ["Owner/Alpha"]);
  f.query.keydown("Enter");
  assert.equal(controller.value(), "Owner/Alpha");
  assert.equal(f.query.value, "Owner/Alpha");
});

test("unmatched text is invalid and never becomes a committed value", () => {
  const f = fixture(["", "one/alpha"]);
  const controller = combo.bind(f.root, f.document);
  f.query.input("invented/repo");
  assert.deepEqual(controller.validate(), {
    valid: false,
    message: "Choose a configured repository or clear the field to use /workspace."
  });
  assert.equal(controller.value(), "");
  assert.equal(f.results.textContent, "No configured repositories match.");
});
```

Add separate tests for ArrowDown/ArrowUp, Home/End, Enter, Escape, Tab exact-match commit, pointerdown selection before blur, reset, pending, outside click, and destroy listener cleanup.

- [ ] **Step 2: Run the new test and confirm it fails**

Run:

```bash
node --test internal/server/web/repository-combobox.test.js
```

Expected: FAIL because `repository-combobox.js` does not exist.

- [ ] **Step 3: Implement the repository combobox controller**

Create `repository-combobox.js` in UMD/CommonJS form. On bind, capture:

```js
var queryInput = root.querySelector("[data-repository-query]");
var committedInput = root.querySelector("[data-start-repository]");
var listbox = root.querySelector("[data-repository-listbox]");
var results = root.querySelector("[data-repository-results]");
var options = Array.from(root.querySelectorAll("[data-repository-option]")).map(function (element) {
  return {element: element, value: element.getAttribute("data-value") || ""};
});
```

Use case-insensitive substring filtering. Keep `activeIndex` and committed canonical value separate from `queryInput.value`. Every query edit clears commitment unless blank or an exact case-insensitive configured match; `validate` uses the exact approved copy. `commit(option)` writes the canonical option value to both the hidden input and visible query (blank renders as an empty query), sets `aria-selected`, closes the popup, and updates the result announcement.

Implement the standard key behavior and set:

- `aria-expanded` on the query;
- `aria-activedescendant` only while an option is active;
- `hidden` on the listbox when closed;
- `aria-selected` on exactly the committed option;
- `results.textContent` to `"N configured repositories available."`, `"N matches."`, or `"No configured repositories match."`.

`setPending(true)` closes the popup and suppresses interaction. `reset()` clears query/commitment and ARIA state. `destroy()` removes every listener.

- [ ] **Step 4: Add failing modal and server-template integration tests**

Update the session modal fixture so `[data-repository-combobox]` contains a visible query, hidden committed input, listbox, result status, and rendered options. Add modal integration cases:

```js
test("unmatched repository blocks fetch and preserves the modal", () => {
  const f = fixture();
  let fetchCalls = 0;
  modalUI.bind(f.document, async () => { fetchCalls++; return response(201, {sessionId:"x"}); });
  f.trigger.dispatch("click");
  f.repositoryQuery.value = "invented/repo";
  f.repositoryQuery.dispatch("input");
  f.form.dispatch("submit");
  assert.equal(fetchCalls, 0);
  assert.equal(f.dialog.open, true);
  assert.equal(f.status.textContent,
    "Choose a configured repository or clear the field to use /workspace.");
});

test("selected autocomplete value is the exact launch repository", async () => {
  const f = fixture();
  const calls = [];
  modalUI.bind(f.document, async (url, options) => {
    calls.push(JSON.parse(options.body));
    return response(201, {sessionId:"created"});
  });
  f.trigger.dispatch("click");
  f.chooseRepository("owner/repo");
  f.form.dispatch("submit");
  await settle();
  assert.equal(calls[0].repository, "owner/repo");
});
```

Change `TestInitialPageRendersSessionModalFromLaunchOptions` to require the combobox/listbox roles, stable IDs, hidden committed input, and `/workspace` option. Assert one option element per configured slug (the escaped slug intentionally appears in both `data-value` and visible text). Reject a remaining `<select id="start-repository">`.

Add `/assets/repository-combobox.js` to every asset table and increase script count from 9 to 10. Assert it loads before `session-modal.js`, has no inline body, and is served with the JavaScript content type.

- [ ] **Step 5: Run integration tests and confirm they fail**

Run:

```bash
node --test --test-name-pattern='repository|unmatched|selected autocomplete' \
  internal/server/web/session-modal.test.js
go test ./internal/server -run 'TestInitialPageRendersSessionModalFromLaunchOptions|TestAstrolabeConsoleIsInteractive|TestHandler' -count=1
```

Expected: FAIL because the modal still renders and reads the native repository select and the new asset is not served.

- [ ] **Step 6: Render and style the accessible combobox**

Replace the repository select in `session-modal.html` with:

```html
<div class="repository-combobox" data-repository-combobox>
  <input id="start-repository" type="text" autocomplete="off"
    role="combobox" aria-autocomplete="list" aria-expanded="false"
    aria-controls="start-repository-list" aria-describedby="start-repository-results"
    placeholder="/workspace" data-repository-query>
  <input type="hidden" value="" data-start-repository>
  <div id="start-repository-list" class="repository-listbox" role="listbox"
    aria-label="Configured repositories" data-repository-listbox hidden>
    <div id="repository-option-workspace" role="option" aria-selected="true"
      data-repository-option data-value="">/workspace</div>
    {{range $index, $repository := .Repositories}}
    <div id="repository-option-{{$index}}" role="option" aria-selected="false"
      data-repository-option data-value="{{$repository.Slug}}">{{$repository.Slug}}</div>
    {{end}}
  </div>
  <div id="start-repository-results" class="repository-results" role="status"
    aria-live="polite" data-repository-results></div>
</div>
```

Style the popup as an anchored, bounded, scrollable Astrolabe listbox. Give active/selected options distinct brass/cyan borders plus text/glyph state, ensure hidden listboxes use `display:none`, and preserve 44-pixel mobile input/option targets. Include the combobox selectors in the existing focus-visible rules.

- [ ] **Step 7: Integrate commitment and validation into the modal**

Change `session-modal.js`'s UMD wrapper to consume the repository module:

```js
if (typeof module === "object" && module.exports) {
  module.exports = factory(require("./repository-combobox.js"));
} else {
  root.KanediasSessionModal = factory(root.KanediasRepositoryCombobox);
}
```

At bind time:

```js
var repositoryRoot = dialog.querySelector("[data-repository-combobox]");
var repository = repositoryCombobox.bind(repositoryRoot, documentObject);
```

Keep `buildRequest`'s wire shape, but read the hidden committed input. In `onSubmit`, validate before `setPending(true)` or `fetchFunction`:

```js
var repositoryValidation = repository.validate();
if (!repositoryValidation.valid) {
  if (status) status.textContent = repositoryValidation.message;
  return;
}
```

Call `repository.reset()` immediately after `form.reset()`, `repository.setPending(pending)` from `setPending`, and `repository.destroy()` from modal `destroy`. Failed HTTP responses must not reset the query/commitment.

Load `/assets/repository-combobox.js` immediately before `/assets/session-modal.js` in `index.html`, and register its embedded handler/route in `handler.go`.

- [ ] **Step 8: Run all modal/autocomplete tests**

Run:

```bash
node --test internal/server/web/repository-combobox.test.js internal/server/web/session-modal.test.js
go test ./internal/server -run 'TestHandler|TestInitialPage|TestAstrolabe|TestProjectStyles' -count=1
git diff --check
```

Expected: PASS.

- [ ] **Step 9: Commit the repository autocomplete deliverable**

```bash
git add internal/server/web/repository-combobox.js internal/server/web/repository-combobox.test.js \
  internal/server/web/session-modal.html internal/server/web/session-modal.js \
  internal/server/web/session-modal.test.js internal/server/web/app.css internal/server/web/index.html \
  internal/server/handler.go internal/server/handler_test.go
git commit -m "feat(ui): autocomplete configured repositories"
```

---

### Task 4: Cross-Feature Regression and Release Verification

**Files:**
- Modify only if a named acceptance assertion below exposes a defect in a Task 1–3 file.
- Test: all Go packages and all `internal/server/web/*.test.js` files.

**Interfaces:**
- Consumes: The three committed deliverables and the approved design at `docs/superpowers/specs/2026-08-10-composer-fleet-repository-usability-design.md`.
- Produces: Verified feature branch with no uncommitted changes and recorded command output suitable for final review.

- [ ] **Step 1: Run formatting and static validation**

```bash
gofmt -w internal/server/handler.go internal/server/handler_test.go
git diff --check
go vet ./...
```

Expected: no output from `git diff --check`; `go vet ./...` exits zero.

- [ ] **Step 2: Run the complete hermetic test suite**

```bash
make test
```

Expected: `go test ./...` and every Node test in `internal/server/web/*.test.js` pass.

- [ ] **Step 3: Run race-sensitive server verification**

```bash
go test -race ./internal/server -count=1
```

Expected: PASS with no race report.

- [ ] **Step 4: Inspect exact acceptance markers in rendered assets**

```bash
go test ./internal/server -run 'TestInitialPage|TestAstrolabe|TestProjectStyles|TestTemplatesDefineStableRoots' -count=1
node --test internal/server/web/fleet-layout.test.js \
  internal/server/web/repository-combobox.test.js \
  internal/server/web/terminal-ui.test.js \
  internal/server/web/app.test.js \
  internal/server/web/session-modal.test.js
```

Expected: PASS, covering textarea semantics, persisted Fleet behavior, mobile transitions, strict combobox commitment, script ordering, and escaped template output.

- [ ] **Step 5: Review branch cleanliness and commit structure**

```bash
git status --short
git log --oneline --decorate main..HEAD
git diff --stat main...HEAD
```

Expected: empty status; one design commit, one plan commit, and three focused implementation commits; the diff contains only the spec, plan, and files named by Tasks 1–3.

- [ ] **Step 6: Commit verification-only adjustments if formatting changed tracked files**

Run this only when Step 1's `gofmt -w` changed `handler.go` or `handler_test.go`:

```bash
git add internal/server/handler.go internal/server/handler_test.go
git commit -m "style: format UX integration tests"
```

If `git status --short` was already empty after Step 1, do not create an empty commit.

---

## Final Review Checklist

- [ ] Composer is visibly two lines at rest and uses 15-pixel text.
- [ ] Composer grows through six lines and then scrolls internally.
- [ ] Enter sends, Shift+Enter inserts a newline, and IME composition is safe.
- [ ] Multiline drafts and attachments remain isolated per selected session.
- [ ] Fleet divider works by pointer and keyboard and reports correct ARIA values.
- [ ] Fleet collapse/restore and preferred width persist only on desktop.
- [ ] Mobile Fleet overlays the variable-height deck and starts closed after reload.
- [ ] Global question navigation reveals a hidden Fleet before scrolling.
- [ ] Repository typing filters only configured slugs and supports complete keyboard use.
- [ ] Blank repository means `/workspace`; unmatched text cannot call the launch endpoint.
- [ ] New assets are local, embedded, CSP-compatible, and load before their consumers.
- [ ] `make test`, server race tests, `go vet`, and `git diff --check` pass.
