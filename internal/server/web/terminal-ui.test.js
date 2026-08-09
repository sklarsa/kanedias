const test = require("node:test");
const assert = require("node:assert/strict");
const ui = require("./terminal-ui.js");

const event = (key, extra = {}) => ({ key, ctrlKey: false, altKey: false, metaKey: false, shiftKey: false, isComposing: false, ...extra });

test("matches Pi editor and interrupt keys without stealing copy", () => {
  assert.equal(ui.keyAction(event("a", {ctrlKey:true}), {target:"deck", hasSelection:false, canInterrupt:false}), "line-start");
  assert.equal(ui.keyAction(event("c", {ctrlKey:true}), {target:"deck", hasSelection:true, canInterrupt:false}), null);
  assert.equal(ui.keyAction(event("c", {ctrlKey:true}), {target:"deck", hasSelection:false, canInterrupt:false}), "clear");
  assert.equal(ui.keyAction(event("Escape"), {target:"deck", hasSelection:false, canInterrupt:true}), "interrupt");
  assert.equal(ui.keyAction(event("Escape"), {target:"deck", hasSelection:false, canInterrupt:false}), null);
  assert.equal(ui.keyAction(event("o", {ctrlKey:true}), {target:"body", hasSelection:false, canInterrupt:false}), "toggle-tools");
});

test("ignores composition, conflicting modifiers, and unrelated editors", () => {
  const deck = {target:"deck", hasSelection:false, canInterrupt:true};
  assert.equal(ui.keyAction(event("Enter"), deck), "submit");
  assert.equal(ui.keyAction(event("Enter"), {...deck, target:"body"}), null);
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
  const document = {
    getElementById(id) {
      return {"main-stack": mainStack, "detail-panel": detail, steerBtn: steer, interruptBtn: interrupt}[id] || null;
    },
    querySelector(selector) { return selector === ".dbtn.stop" ? stop : null; }
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
  for (const control of [steer, interrupt, stop]) {
    assert.equal(control.disabled, true);
    assert.equal(control.getAttribute("aria-disabled"), "true");
    assert.equal(control.classList.contains("armed"), false);
  }

  detail.setAttribute("data-can-steer", "true");
  detail.setAttribute("data-can-interrupt", "true");
  detail.setAttribute("data-can-stop", "true");
  observer.callback([{type: "attributes", target: detail}]);
  for (const control of [steer, interrupt, stop]) {
    assert.equal(control.disabled, false);
    assert.equal(control.getAttribute("aria-disabled"), "false");
    assert.equal(control.classList.contains("armed"), true);
  }
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

test("clear dispatches a bubbling input event and focuses the deck", () => {
  const dispatched = [];
  const input = {
    value: "queued",
    dispatchEvent(value) { dispatched.push(value); },
    focus() { this.focused = true; }
  };
  class FakeEvent {
    constructor(type, options) { this.type = type; this.bubbles = options.bubbles; }
  }
  const keyEvent = {preventDefault() { this.prevented = true; }};
  const document = {querySelector: selector => selector === ".deck-input" ? input : null};
  ui.performAction("clear", {event: keyEvent, document, Event: FakeEvent});
  assert.equal(keyEvent.prevented, true);
  assert.equal(input.value, "");
  assert.equal(input.focused, true);
  assert.equal(dispatched.length, 1);
  assert.equal(dispatched[0].type, "input");
  assert.equal(dispatched[0].bubbles, true);
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
