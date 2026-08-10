const test = require("node:test");
const assert = require("node:assert/strict");
const ui = require("./terminal-ui.js");

const event = (key, extra = {}) => ({ key, ctrlKey: false, altKey: false, metaKey: false, shiftKey: false, isComposing: false, ...extra });

test("matches terminal editor and interrupt keys without stealing copy", () => {
  assert.equal(ui.keyAction(event("a", {ctrlKey:true}), {target:"deck", hasSelection:false, canInterrupt:false}), "select-all");
  assert.equal(ui.keyAction(event("a", {ctrlKey:true}), {target:"body", hasSelection:false, canInterrupt:false}), null);
  assert.equal(ui.keyAction(event("c", {ctrlKey:true}), {target:"deck", hasSelection:true, canInterrupt:false}), null);
  assert.equal(ui.keyAction(event("c", {ctrlKey:true}), {target:"deck", hasSelection:false, canInterrupt:false}), "clear");
  assert.equal(ui.keyAction(event("Escape"), {target:"deck", hasSelection:false, canInterrupt:true}), "interrupt");
  assert.equal(ui.keyAction(event("Escape"), {target:"deck", hasSelection:false, canInterrupt:false}), null);
  assert.equal(ui.keyAction(event("o", {ctrlKey:true}), {target:"body", hasSelection:false, canInterrupt:false}), "toggle-tools");
});

test("bare Enter submits while Shift-Enter remains a textarea newline", () => {
  const deck = {target:"deck", hasSelection:false, canInterrupt:true};
  assert.equal(ui.keyAction(event("Enter"), deck), "submit");
  assert.equal(ui.keyAction(event("Enter", {shiftKey:true}), deck), null);
  assert.equal(ui.keyAction(event("Enter", {isComposing:true}), deck), null);
  assert.equal(ui.keyAction(event("Enter"), {...deck, target:"body"}), null);
});

test("IME keyCode 229 Enter is ignored without changing the following normal Enter", () => {
  const deck = {target:"deck", hasSelection:false, canInterrupt:true};
  const compositionEnter = event("Enter", {keyCode: 229});
  assert.equal(compositionEnter.isComposing, false);
  assert.equal(ui.keyAction(compositionEnter, deck), null);
  assert.equal(ui.keyAction(event("Enter", {keyCode: 13}), deck), "submit");
});

test("ignores composition, conflicting modifiers, and unrelated editors", () => {
  const deck = {target:"deck", hasSelection:false, canInterrupt:true};
  assert.equal(ui.keyAction(event("a", {ctrlKey:true, isComposing:true}), deck), null);
  assert.equal(ui.keyAction(event("a", {ctrlKey:true, shiftKey:true}), deck), null);
  assert.equal(ui.keyAction(event("a", {ctrlKey:true, altKey:true}), deck), null);
  assert.equal(ui.keyAction(event("a", {ctrlKey:true, metaKey:true}), deck), null);
  assert.equal(ui.keyAction(event("Escape", {altKey:true}), deck), null);
  assert.equal(ui.keyAction(event("Escape", {metaKey:true}), deck), null);
  assert.equal(ui.keyAction(event("c", {ctrlKey:true}), {...deck, target:"other-editable"}), null);
  assert.equal(ui.keyAction(event("c", {ctrlKey:true}), {...deck, target:"body"}), "clear");
  assert.equal(ui.keyAction(event("o", {ctrlKey:true}), {...deck, target:"other-editable"}), null);
});

test("global tool toggle expands unless every card is open", () => {
  assert.equal(ui.nextToolExpansion([]), true);
  assert.equal(ui.nextToolExpansion([true, false]), true);
  assert.equal(ui.nextToolExpansion([true, true]), false);
});

function fakeControl() {
  const classes = new Set();
  const attributes = new Map();
  return {
    disabled: false,
    classList: {
      toggle(name, enabled) {
        if (enabled) classes.add(name); else classes.delete(name);
      },
      contains(name) { return classes.has(name); }
    },
    setAttribute(name, value) { attributes.set(name, value); },
    getAttribute(name) { return attributes.get(name) ?? null; }
  };
}

test("same-node capability attribute morph synchronizes controls", () => {
  const detailAttributes = new Map([
    ["data-can-steer", "false"],
    ["data-can-interrupt", "false"],
    ["data-can-stop", "false"]
  ]);
  const detail = {
    getAttribute(name) { return detailAttributes.get(name) ?? null; },
    setAttribute(name, value) { detailAttributes.set(name, value); }
  };
  const mainStack = {};
  const steer = fakeControl();
  const interrupt = fakeControl();
  const stop = fakeControl();
  const attach = fakeControl();
  const fileInput = fakeControl();
  const deckInput = fakeControl();
  let deckBusy = false;
  const deck = {getAttribute(name) { return name === "aria-busy" ? String(deckBusy) : null; }};
  const document = {
    getElementById(id) {
      return {
        "main-stack": mainStack,
        "detail-panel": detail,
        steerBtn: steer,
        interruptBtn: interrupt,
        "attach-images-button": attach,
        "image-file-input": fileInput
      }[id] || null;
    },
    querySelector(selector) {
      if (selector === ".dbtn.stop") return stop;
      if (selector === ".deck") return deck;
      if (selector === ".deck-input") return deckInput;
      return null;
    }
  };
  class FakeMutationObserver {
    constructor(callback) { this.callback = callback; }
    observe(target, options) { this.target = target; this.options = options; }
  }

  const observer = ui.observeDeckCapabilities(document, FakeMutationObserver);
  assert.equal(observer.target, mainStack);
  assert.deepEqual(observer.options, {
    childList: true,
    subtree: true,
    attributes: true,
    attributeFilter: ["data-can-steer", "data-can-interrupt", "data-can-stop"]
  });
  for (const control of [steer, interrupt, stop, attach, fileInput, deckInput]) {
    assert.equal(control.disabled, true);
    assert.equal(control.getAttribute("aria-disabled"), "true");
    assert.equal(control.classList.contains("armed"), false);
  }

  detail.setAttribute("data-can-steer", "true");
  detail.setAttribute("data-can-interrupt", "true");
  detail.setAttribute("data-can-stop", "true");
  observer.callback([{type: "attributes", target: detail}]);
  for (const control of [steer, interrupt, stop, attach, fileInput, deckInput]) {
    assert.equal(control.disabled, false);
    assert.equal(control.getAttribute("aria-disabled"), "false");
    assert.equal(control.classList.contains("armed"), true);
  }

  deckBusy = true;
  ui.syncDeckState(document);
  for (const control of [steer, attach, fileInput, deckInput]) {
    assert.equal(control.disabled, true);
    assert.equal(control.getAttribute("aria-disabled"), "true");
  }
  assert.equal(interrupt.disabled, false);
  assert.equal(stop.disabled, false);
});

test("selection detection preserves non-text and input ranges", () => {
  const nonTextSelection = {isCollapsed: false, toString() { return ""; }};
  assert.equal(ui.hasTextSelection({getSelection: () => nonTextSelection}, null), true);
  assert.equal(ui.hasTextSelection({getSelection: () => ({isCollapsed: true})}, {
    tagName: "INPUT", selectionStart: 1, selectionEnd: 2
  }), true);
  assert.equal(ui.hasTextSelection({getSelection: () => ({isCollapsed: true})}, {
    tagName: "TEXTAREA", selectionStart: 2, selectionEnd: 2
  }), false);
});

test("Escape works from body only when authorized and never hijacks contenteditable", () => {
  const body = {closest() { return null; }};
  const contenteditable = {
    closest(selector) { return selector.includes("[contenteditable]") ? this : null; }
  };
  assert.equal(ui.keyboardTarget(body), "body");
  assert.equal(ui.keyboardTarget(contenteditable), "other-editable");
  assert.equal(ui.keyAction(event("Escape"), {
    target: ui.keyboardTarget(body), hasSelection: false, canInterrupt: true
  }), "interrupt");
  assert.equal(ui.keyAction(event("Escape"), {
    target: ui.keyboardTarget(body), hasSelection: false, canInterrupt: false
  }), null);
  assert.equal(ui.keyAction(event("Escape"), {
    target: ui.keyboardTarget(contenteditable), hasSelection: false, canInterrupt: true
  }), null);

  let clicks = 0;
  const button = {disabled: true, click() { clicks++; }};
  const document = {getElementById: id => id === "interruptBtn" ? button : null};
  const keyEvent = {preventDefault() { this.prevented = true; }};
  ui.performAction("interrupt", {event: keyEvent, document});
  assert.equal(keyEvent.prevented, true);
  assert.equal(clicks, 0);
  button.disabled = false;
  ui.performAction("interrupt", {event: keyEvent, document});
  assert.equal(clicks, 1);
});

function fakeSubmitDocument(disabled) {
  const dispatched = [];
  const input = {
    value: "queued",
    dispatchEvent(value) { dispatched.push(value); },
    focus() { this.focused = true; }
  };
  const steer = {
    disabled,
    clicks: 0,
    click() { if (!this.disabled) this.clicks++; }
  };
  return {
    dispatched,
    input,
    steer,
    document: {
      getElementById: id => id === "steerBtn" ? steer : null,
      querySelector: selector => selector === ".deck-input" ? input : null
    }
  };
}

class FakeInputEvent {
  constructor(type, options) { this.type = type; this.bubbles = options.bubbles; }
}

function performSubmit(fixture, submit) {
  const keyEvent = {preventDefault() { this.prevented = true; }};
  ui.performAction("submit", {
    event: keyEvent,
    document: fixture.document,
    Event: FakeInputEvent,
    submit
  });
  return keyEvent;
}

test("Enter delegates submission without clearing before acceptance", () => {
  const fixture = fakeSubmitDocument(false);
  fixture.input.value = "inspect this";
  let submits = 0;
  const keyEvent = performSubmit(fixture, () => { submits++; });
  assert.equal(keyEvent.prevented, true);
  assert.equal(submits, 1);
  assert.equal(fixture.steer.clicks, 0);
  assert.equal(fixture.input.value, "inspect this");
  assert.equal(fixture.dispatched.length, 0);
});

test("submit falls back to enabled Steer without optimistic clear", () => {
  const fixture = fakeSubmitDocument(false);
  performSubmit(fixture);
  assert.equal(fixture.steer.clicks, 1);
  assert.equal(fixture.input.value, "queued");
  assert.equal(fixture.dispatched.length, 0);

  const disabled = fakeSubmitDocument(true);
  performSubmit(disabled);
  assert.equal(disabled.steer.clicks, 0);
  assert.equal(disabled.input.value, "queued");
});

test("Ctrl-A selects the complete deck input", () => {
  const fixture = fakeSubmitDocument(false);
  fixture.input.value = "select this directive";
  fixture.input.setSelectionRange = function (start, end) {
    this.selectionStart = start;
    this.selectionEnd = end;
  };
  const keyEvent = {preventDefault() { this.prevented = true; }};
  const action = ui.keyAction(event("a", {ctrlKey: true}), {
    target: "deck", hasSelection: false, canInterrupt: false
  });

  ui.performAction(action, {event: keyEvent, document: fixture.document});

  assert.equal(keyEvent.prevented, true);
  assert.equal(fixture.input.focused, true);
  assert.equal(fixture.input.selectionStart, 0);
  assert.equal(fixture.input.selectionEnd, fixture.input.value.length);
});

test("clear dispatches a bubbling input event and focuses the deck", () => {
  const fixture = fakeSubmitDocument(false);
  const keyEvent = {preventDefault() { this.prevented = true; }};
  ui.performAction("clear", {event: keyEvent, document: fixture.document, Event: FakeInputEvent});
  assert.equal(keyEvent.prevented, true);
  assert.equal(fixture.input.value, "");
  assert.equal(fixture.input.focused, true);
  assert.equal(fixture.dispatched.length, 1);
  assert.equal(fixture.dispatched[0].type, "input");
  assert.equal(fixture.dispatched[0].bubbles, true);
});

function makeFakeTimer() {
  let nextId = 0;
  const scheduled = new Map();
  return {
    setTimeout(fn, delay) {
      const id = ++nextId;
      scheduled.set(id, { fn, delay });
      return id;
    },
    clearTimeout(id) { scheduled.delete(id); },
    fire(id) {
      const task = scheduled.get(id);
      if (!task) return false;
      scheduled.delete(id);
      task.fn();
      return true;
    },
    pending() { return Array.from(scheduled.entries()); },
    count() { return scheduled.size; }
  };
}

// A minimal stand-in for #deck-status. The transient success span (.deck-ok)
// carries data-success-id and is removable by the controller; the error node
// (.deck-error) lives outside that span and is never removed, so errors persist.
function makeDeckStatusDoc() {
  const data = { id: null, present: false, errorPresent: false };
  const span = {
    getAttribute(name) { return name === "data-success-id" ? data.id : null; }
  };
  span.parentNode = {
    removeChild(child) { if (child === span) data.present = false; }
  };
  const errorNode = { getAttribute() { return null; } };
  return {
    setSuccess(id) { data.id = id; data.present = true; data.errorPresent = false; },
    setError() { data.id = null; data.present = false; data.errorPresent = true; },
    present() { return data.present; },
    errorPresent() { return data.errorPresent; },
    querySelector(sel) {
      if (sel === "#deck-status .deck-ok") return data.present ? span : null;
      if (sel === "#deck-status .deck-error") return data.errorPresent ? errorNode : null;
      return null;
    }
  };
}

function makeDeckCtrl(timer) {
  return ui.createDeckStatusController({
    delay: 2000,
    setTimeout: timer.setTimeout,
    clearTimeout: timer.clearTimeout
  });
}

test("deck status success auto-clears after exactly the delay (2000ms)", () => {
  const timer = makeFakeTimer();
  const ctrl = makeDeckCtrl(timer);
  const doc = makeDeckStatusDoc();
  doc.setSuccess("ack-1");
  ctrl.schedule(doc);
  assert.equal(timer.count(), 1);
  const [id, task] = timer.pending()[0];
  assert.equal(task.delay, 2000);
  assert.equal(doc.present(), true);
  timer.fire(id);
  assert.equal(doc.present(), false);
  assert.equal(timer.count(), 0);
});

test("deck status error is never auto-cleared", () => {
  const timer = makeFakeTimer();
  const ctrl = makeDeckCtrl(timer);
  const doc = makeDeckStatusDoc();
  // Error-only state: no .deck-ok is present, so nothing is scheduled or
  // removed and the transient error content stays in place.
  ctrl.schedule(doc);
  assert.equal(timer.count(), 0);
  assert.equal(ctrl.successID(doc), null);
});

test("repeated identical success restarts the lifetime", () => {
  const timer = makeFakeTimer();
  const ctrl = makeDeckCtrl(timer);
  const doc = makeDeckStatusDoc();
  doc.setSuccess("ack-1");
  ctrl.schedule(doc);
  const [firstId] = timer.pending()[0];
  // The same marker reappears (morph): it must restart, not accumulate.
  ctrl.schedule(doc);
  assert.equal(timer.count(), 1);
  timer.fire(firstId); // cancelled first timer is a no-op
  assert.equal(doc.present(), true);
  const [secondId] = timer.pending()[0];
  timer.fire(secondId);
  assert.equal(doc.present(), false);
});

test("a stale success callback never clears a newer success", () => {
  const timer = makeFakeTimer();
  const ctrl = makeDeckCtrl(timer);
  const doc = makeDeckStatusDoc();
  doc.setSuccess("ack-1");
  ctrl.schedule(doc);
  const [, firstTask] = timer.pending()[0];
  // A newer success with a distinct marker supersedes the prior one.
  doc.setSuccess("ack-2");
  ctrl.schedule(doc);
  assert.equal(timer.count(), 1);
  // Even if the older callback fires after cancellation it must not clear
  // the newer success.
  firstTask.fn();
  assert.equal(doc.present(), true);
  const [secondId] = timer.pending()[0];
  timer.fire(secondId);
  assert.equal(doc.present(), false);
});

test("deck status error never auto-clears an actual .deck-error node", () => {
  const timer = makeFakeTimer();
  const ctrl = makeDeckCtrl(timer);
  const doc = makeDeckStatusDoc();
  // A real error node with transient success content absent: nothing is
  // scheduled or removed, and the error persists.
  doc.setError();
  ctrl.schedule(doc);
  assert.equal(timer.count(), 0);
  assert.equal(ctrl.successID(doc), null);
  assert.equal(doc.present(), false);
  assert.equal(doc.errorPresent(), true);
});

test("deck status error cancels a pending success timer and persists", () => {
  const timer = makeFakeTimer();
  const ctrl = makeDeckCtrl(timer);
  const doc = makeDeckStatusDoc();
  // Begin as a pending success (auto-clear scheduled).
  doc.setSuccess("ack-1");
  ctrl.schedule(doc);
  assert.equal(timer.count(), 1);
  const [, firstTask] = timer.pending()[0];
  assert.equal(doc.present(), true);
  // Transition to an actual .deck-error: the pending success timer must be
  // canceled so the error is never auto-cleared.
  doc.setError();
  ctrl.schedule(doc);
  assert.equal(timer.count(), 0);
  // Even if the stale captured callback fires after cancellation, it is a
  // no-op and must not remove the error node.
  firstTask.fn();
  assert.equal(doc.errorPresent(), true);
  assert.equal(doc.present(), false);
});

test("Ctrl-O with no cards primes a fresh patched card", () => {
  let cards = [];
  const root = {querySelectorAll: selector => selector === "[data-tool-card]" ? cards : []};
  const tools = ui.createToolExpansionController();
  const keyEvent = {preventDefault() { this.prevented = true; }};
  const action = ui.keyAction(event("o", {ctrlKey: true}), {
    target: "body", hasSelection: false, canInterrupt: false
  });
  ui.performAction(action, {event: keyEvent, document: root, tools});
  assert.equal(keyEvent.prevented, true);
  assert.equal(tools.mode(), true);

  const fresh = {open: false};
  cards = [fresh];
  tools.refresh(root);
  assert.equal(fresh.open, true);
});

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
