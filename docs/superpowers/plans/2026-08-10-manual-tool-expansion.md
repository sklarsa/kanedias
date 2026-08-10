# Manual Tool Expansion During Streaming Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve a user's manual open or closed choice for an existing tool-result card while streamed activity continues, without changing Ctrl-O behavior for current or newly inserted cards.

**Architecture:** Keep the native `<details>.open` property as the owner of per-card state. The tool-expansion controller will use a `WeakSet` to distinguish existing cards from newly inserted cards: explicit Ctrl-O commands update every current card, while MutationObserver refreshes apply the stored global mode only to unseen cards.

**Tech Stack:** Browser JavaScript, native `WeakSet`, native `<details>` state, Node.js built-in test runner.

## Global Constraints

- Ctrl-O must continue to expand or collapse every current tool card.
- Newly inserted cards must inherit the most recent Ctrl-O mode.
- Manual toggles on existing cards must survive subsequent streamed mutations.
- A later Ctrl-O command may intentionally overwrite manual per-card choices.
- Do not change the activity template, SSE protocol, or vendored Datastar asset.
- Add no new dependency.

---

### Task 1: Preserve manual state across streaming refreshes

**Files:**
- Modify: `internal/server/web/terminal-ui.js:88-113`
- Test: `internal/server/web/terminal-ui.test.js:231-247`

**Interfaces:**
- Consumes: `createToolExpansionController()`, roots exposing `querySelectorAll("[data-tool-card]")`, and card objects exposing boolean `open` state.
- Produces: unchanged controller methods `mode(): boolean|null`, `refresh(root): void`, and `toggle(root): boolean`, with refresh applying global state only to cards not previously encountered.

- [ ] **Step 1: Add regressions for manual overrides and new-card inheritance**

Append these tests after `Ctrl-O with no cards primes a fresh patched card` in `internal/server/web/terminal-ui.test.js`:

```js
function fakeToolRoot(initialCards) {
  const cards = initialCards.slice();
  return {
    cards,
    querySelectorAll(selector) {
      return selector === "[data-tool-card]" ? cards : [];
    }
  };
}

test("stream refresh preserves a manual open after global collapse", () => {
  const existing = {open: true};
  const root = fakeToolRoot([existing]);
  const tools = ui.createToolExpansionController();

  assert.equal(tools.toggle(root), false);
  assert.equal(existing.open, false);

  existing.open = true;
  tools.refresh(root);
  assert.equal(existing.open, true);

  const fresh = {open: true};
  root.cards.push(fresh);
  tools.refresh(root);
  assert.equal(existing.open, true);
  assert.equal(fresh.open, false);
});

test("stream refresh preserves a manual close after global expansion", () => {
  const existing = {open: false};
  const root = fakeToolRoot([existing]);
  const tools = ui.createToolExpansionController();

  assert.equal(tools.toggle(root), true);
  assert.equal(existing.open, true);

  existing.open = false;
  tools.refresh(root);
  assert.equal(existing.open, false);
});

test("a later global toggle still updates manually overridden cards", () => {
  const first = {open: true};
  const second = {open: true};
  const root = fakeToolRoot([first, second]);
  const tools = ui.createToolExpansionController();

  assert.equal(tools.toggle(root), false);
  first.open = true;
  tools.refresh(root);
  assert.equal(first.open, true);

  assert.equal(tools.toggle(root), true);
  assert.equal(first.open, true);
  assert.equal(second.open, true);
});
```

- [ ] **Step 2: Run the focused tests and verify RED**

Run:

```bash
node --test --test-name-pattern='stream refresh|later global toggle' internal/server/web/terminal-ui.test.js
```

Expected: the two `stream refresh preserves` tests fail because `refresh` resets the manually changed existing card to `expansionMode`; the later-global-toggle test passes or remains available as a compatibility contract.

- [ ] **Step 3: Make refresh initialize only unseen cards**

Replace `createToolExpansionController` in `internal/server/web/terminal-ui.js` with:

```js
function createToolExpansionController() {
  var expansionMode = null;
  var seenCards = new WeakSet();

  function cardsIn(root) {
    return Array.prototype.slice.call(root.querySelectorAll("[data-tool-card]"));
  }

  function apply(card) {
    seenCards.add(card);
    if (card.open !== expansionMode) card.open = expansionMode;
  }

  function refresh(root) {
    var cards = cardsIn(root);
    for (var i = 0; i < cards.length; i++) {
      if (seenCards.has(cards[i])) continue;
      seenCards.add(cards[i]);
      if (expansionMode !== null) apply(cards[i]);
    }
  }

  function toggle(root) {
    var cards = cardsIn(root);
    expansionMode = nextToolExpansion(cards.map(function (card) { return card.open; }));
    for (var i = 0; i < cards.length; i++) apply(cards[i]);
    return expansionMode;
  }

  return {
    mode: function () { return expansionMode; },
    refresh: refresh,
    toggle: toggle
  };
}
```

The explicit loop in `toggle` is required: unlike refresh, Ctrl-O intentionally updates both seen and unseen cards.

- [ ] **Step 4: Run the browser-unit suite and verify GREEN**

Run:

```bash
node --test internal/server/web/terminal-ui.test.js
```

Expected: PASS, including existing keyboard, Ctrl-O, capability, and input behavior.

- [ ] **Step 5: Commit the implementation**

```bash
git add internal/server/web/terminal-ui.js internal/server/web/terminal-ui.test.js
git commit -m "fix(ui): preserve manual tool expansion while streaming"
```

---

### Task 2: Verify the integrated change

**Files:**
- Verify: `internal/server/web/terminal-ui.js`
- Verify: `internal/server/web/terminal-ui.test.js`

**Interfaces:**
- Consumes: the updated tool-expansion controller and all repository tests.
- Produces: review evidence that the focused bug is fixed without changing server rendering or unrelated behavior.

- [ ] **Step 1: Run every browser-unit test**

Run:

```bash
node --test internal/server/web/*.test.js
```

Expected: PASS.

- [ ] **Step 2: Run the complete Go test suite without cache**

Run:

```bash
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 3: Run the repository test target**

Run:

```bash
make test
```

Expected: PASS for all Go packages and browser-unit tests.

- [ ] **Step 4: Check scope and whitespace**

Run:

```bash
git diff --check main...HEAD
git status --short
git diff --stat main...HEAD
git diff main...HEAD -- internal/server/web/terminal-ui.js internal/server/web/terminal-ui.test.js
```

Expected: no whitespace errors, a clean worktree, and only the approved controller and test changes beyond the committed design and plan documents.
