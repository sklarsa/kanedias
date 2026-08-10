"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const combo = require("./repository-combobox.js");

class FakeTarget {
  constructor() {
    this.listeners = new Map();
    this.attributes = new Map();
    this.hidden = false;
    this.disabled = false;
    this.textContent = "";
    this.parent = null;
  }
  addEventListener(type, listener, options) {
    const listeners = this.listeners.get(type) || [];
    listeners.push({ listener, capture: options === true || Boolean(options && options.capture) });
    this.listeners.set(type, listeners);
  }
  removeEventListener(type, listener, options) {
    const capture = options === true || Boolean(options && options.capture);
    this.listeners.set(type, (this.listeners.get(type) || []).filter(
      (item) => item.listener !== listener || item.capture !== capture
    ));
  }
  dispatch(type, init = {}) {
    const event = Object.assign({
      type,
      target: this,
      defaultPrevented: false,
      propagationStopped: false,
      immediatePropagationStopped: false,
      preventDefault() { this.defaultPrevented = true; },
      stopPropagation() { this.propagationStopped = true; },
      stopImmediatePropagation() {
        this.immediatePropagationStopped = true;
        this.propagationStopped = true;
      }
    }, init);
    for (const phase of [true, false]) {
      for (const item of this.listeners.get(type) || []) {
        if (item.capture !== phase || event.immediatePropagationStopped) continue;
        item.listener(event);
      }
    }
    return event;
  }
  getAttribute(name) { return this.attributes.has(name) ? this.attributes.get(name) : null; }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  removeAttribute(name) { this.attributes.delete(name); }
  contains(target) {
    for (let current = target; current; current = current.parent) {
      if (current === this) return true;
    }
    return false;
  }
}

class FakeInput extends FakeTarget {
  constructor(document, value = "") {
    super();
    this.document = document;
    this.value = value;
    this.defaultValue = value;
  }
  focus() {
    this.document.activeElement = this;
    this.dispatch("focus");
  }
  input(value) {
    this.value = value;
    return this.dispatch("input");
  }
  keydown(key, extra = {}) { return this.dispatch("keydown", { key, ...extra }); }
  blur(relatedTarget = null) {
    this.document.activeElement = relatedTarget;
    return this.dispatch("blur", { relatedTarget });
  }
}

function fixture(values) {
  const document = new FakeTarget();
  document.activeElement = null;
  const root = new FakeTarget();
  const query = new FakeInput(document);
  const committed = new FakeInput(document);
  const listbox = new FakeTarget();
  const results = new FakeTarget();
  const empty = new FakeTarget();
  root.parent = document;
  query.parent = root;
  committed.parent = root;
  listbox.parent = root;
  results.parent = root;
  empty.parent = listbox;
  empty.hidden = true;
  empty.textContent = "No configured repositories match.";
  empty.setAttribute("role", "presentation");
  listbox.hidden = true;
  query.setAttribute("aria-expanded", "false");
  const options = values.map((value, index) => {
    const element = new FakeTarget();
    element.parent = listbox;
    element.textContent = value || "/workspace";
    element.setAttribute("id", "repository-option-" + index);
    element.setAttribute("data-value", value);
    element.setAttribute("aria-selected", value === "" ? "true" : "false");
    return element;
  });
  const bySelector = new Map([
    ["[data-repository-query]", query],
    ["[data-start-repository]", committed],
    ["[data-repository-listbox]", listbox],
    ["[data-repository-results]", results],
    ["[data-repository-empty]", empty]
  ]);
  root.querySelector = (selector) => bySelector.get(selector) || null;
  root.querySelectorAll = (selector) => selector === "[data-repository-option]" ? options : [];

  return {
    document, root, query, committed, listbox, results, empty, options,
    visibleValues() {
      return options.filter((item) => !item.hidden).map((item) => item.getAttribute("data-value") || "");
    },
    selectedValues() {
      return options.filter((item) => item.getAttribute("aria-selected") === "true")
        .map((item) => item.getAttribute("data-value") || "");
    }
  };
}

test("blank query commits workspace and focus shows every option", () => {
  const f = fixture(["", "one/alpha", "two/beta"]);
  const controller = combo.bind(f.root, f.document);
  f.query.focus();
  assert.equal(controller.value(), "");
  assert.deepEqual(f.visibleValues(), ["", "one/alpha", "two/beta"]);
  assert.equal(f.query.getAttribute("aria-expanded"), "true");
  assert.equal(f.results.textContent, "3 configured repositories available.");
});

test("filtering is case-insensitive but commits canonical server spelling", () => {
  const f = fixture(["", "Owner/Alpha", "two/beta"]);
  const controller = combo.bind(f.root, f.document);
  f.query.input("owner/a");
  assert.deepEqual(f.visibleValues(), ["Owner/Alpha"]);
  f.query.keydown("Enter");
  assert.equal(controller.value(), "Owner/Alpha");
  assert.equal(f.query.value, "Owner/Alpha");
  assert.deepEqual(f.selectedValues(), ["Owner/Alpha"]);
});

test("unmatched text is invalid and displays a non-option no-results row", () => {
  const f = fixture(["", "one/alpha"]);
  const controller = combo.bind(f.root, f.document);
  f.query.input("invented/repo");
  assert.deepEqual(controller.validate(), {
    valid: false,
    message: "Choose a configured repository or clear the field to use /workspace."
  });
  assert.equal(controller.value(), "");
  assert.equal(f.results.textContent, "No configured repositories match.");
  assert.equal(f.empty.hidden, false);
  assert.equal(f.empty.getAttribute("role"), "presentation");
  assert.equal(f.root.querySelectorAll("[data-repository-option]").includes(f.empty), false);
  f.query.input("one");
  assert.equal(f.empty.hidden, true);
});

test("Arrow keys, Home, and End move exactly one active option through open results", () => {
  const f = fixture(["", "one/alpha", "one/beta", "two/gamma"]);
  combo.bind(f.root, f.document);
  f.query.input("one/");
  f.query.keydown("ArrowDown");
  assert.equal(f.query.getAttribute("aria-activedescendant"), "repository-option-1");
  f.query.keydown("ArrowDown");
  assert.equal(f.query.getAttribute("aria-activedescendant"), "repository-option-2");
  assert.deepEqual(f.options.filter((option) => option.getAttribute("data-active") === "true"), [f.options[2]]);
  f.query.keydown("ArrowUp");
  assert.equal(f.query.getAttribute("aria-activedescendant"), "repository-option-1");
  assert.deepEqual(f.options.filter((option) => option.getAttribute("data-active") === "true"), [f.options[1]]);
  f.query.keydown("End");
  assert.equal(f.query.getAttribute("aria-activedescendant"), "repository-option-2");
  f.query.keydown("Home");
  assert.equal(f.query.getAttribute("aria-activedescendant"), "repository-option-1");
});

test("Home and End retain native caret behavior while the popup is closed", () => {
  const f = fixture(["", "one/alpha"]);
  combo.bind(f.root, f.document);
  const home = f.query.keydown("Home");
  const end = f.query.keydown("End");
  assert.equal(home.defaultPrevented, false);
  assert.equal(end.defaultPrevented, false);
  assert.equal(f.listbox.hidden, true);
  assert.equal(f.query.getAttribute("aria-activedescendant"), null);
});

test("Enter commits the active option and Escape closes without inventing a value", () => {
  const f = fixture(["", "one/alpha", "one/beta"]);
  const controller = combo.bind(f.root, f.document);
  f.query.input("one/");
  f.query.keydown("ArrowDown");
  const enter = f.query.keydown("Enter");
  assert.equal(enter.defaultPrevented, true);
  assert.equal(controller.value(), "one/alpha");
  assert.equal(f.listbox.hidden, true);
  assert.equal(f.query.getAttribute("aria-activedescendant"), null);

  f.query.input("invented");
  const escape = f.query.keydown("Escape");
  assert.equal(escape.defaultPrevented, true);
  assert.equal(escape.propagationStopped, true);
  assert.equal(f.listbox.hidden, true);
  assert.equal(controller.value(), "");
});

test("Tab commits an exact case-insensitive match and only closes an unmatched query", () => {
  const f = fixture(["", "Owner/Alpha"]);
  const controller = combo.bind(f.root, f.document);
  f.query.input("owner/alpha");
  const exactTab = f.query.keydown("Tab");
  assert.equal(exactTab.defaultPrevented, false);
  assert.equal(controller.value(), "Owner/Alpha");
  assert.equal(f.query.value, "Owner/Alpha");

  f.query.input("Owner/Al");
  f.query.keydown("Tab");
  assert.equal(controller.value(), "");
  assert.equal(f.query.value, "Owner/Al");
  assert.equal(f.listbox.hidden, true);
});

test("mouse pointerdown commits before blur can close the listbox", () => {
  const f = fixture(["", "one/alpha"]);
  const controller = combo.bind(f.root, f.document);
  f.query.input("one");
  const pointer = f.options[1].dispatch("pointerdown", {pointerType: "mouse", pointerId: 1});
  f.query.blur();
  assert.equal(pointer.defaultPrevented, true);
  assert.equal(controller.value(), "one/alpha");
  assert.equal(f.query.value, "one/alpha");
});

test("touch and pen tap commit on pointerup while a pan never chooses an option", () => {
  for (const pointerType of ["touch", "pen"]) {
    const tap = fixture(["", "one/alpha"]);
    const tapController = combo.bind(tap.root, tap.document);
    tap.query.input("one");
    const down = tap.options[1].dispatch("pointerdown", {
      pointerType, pointerId: 7, clientX: 10, clientY: 20
    });
    tap.query.blur();
    assert.equal(down.defaultPrevented, false);
    assert.equal(tap.listbox.hidden, false);
    tap.document.dispatch("pointerup", {
      target: tap.options[1], pointerType, pointerId: 7, clientX: 12, clientY: 23
    });
    assert.equal(tapController.value(), "one/alpha");

    const pan = fixture(["", "one/alpha"]);
    const panController = combo.bind(pan.root, pan.document);
    pan.query.input("one");
    pan.options[1].dispatch("pointerdown", {
      pointerType, pointerId: 8, clientX: 10, clientY: 20
    });
    pan.document.dispatch("pointermove", {
      target: pan.options[1], pointerType, pointerId: 8, clientX: 10, clientY: 45
    });
    pan.document.dispatch("pointerup", {
      target: pan.options[1], pointerType, pointerId: 8, clientX: 10, clientY: 45
    });
    assert.equal(panController.value(), "");
    assert.equal(pan.query.value, "one");
  }
});

test("composition ordering protects Enter and Escape, then restores normal two-step keys", () => {
  const f = fixture(["", "one/alpha"]);
  const controller = combo.bind(f.root, f.document);
  f.query.input("one");
  f.query.dispatch("compositionstart");
  f.query.keydown("Enter", {isComposing: false, keyCode: 229});
  assert.equal(controller.value(), "");
  f.query.dispatch("compositionend");
  f.query.keydown("Enter", {isComposing: false, keyCode: 13});
  assert.equal(controller.value(), "", "trailing composition key committed");
  f.query.keydown("Enter", {isComposing: false, keyCode: 13});
  assert.equal(controller.value(), "one/alpha");

  f.query.input("one");
  f.query.dispatch("compositionstart");
  f.query.dispatch("compositionend");
  const trailingEscape = f.query.keydown("Escape");
  assert.equal(trailingEscape.defaultPrevented, false);
  assert.equal(f.listbox.hidden, false);
  const normalEscape = f.query.keydown("Escape");
  assert.equal(normalEscape.defaultPrevented, true);
  assert.equal(f.listbox.hidden, true);
});

test("composition guard expires before a later intentional Enter", async () => {
  const f = fixture(["", "one/alpha"]);
  const controller = combo.bind(f.root, f.document);
  f.query.input("one");
  f.query.dispatch("compositionstart");
  f.query.dispatch("compositionend");
  await new Promise((resolve) => setTimeout(resolve, 0));
  f.query.keydown("Enter");
  assert.equal(controller.value(), "one/alpha");
});

test("closure commitment stays authoritative when the hidden mirror drifts", () => {
  const f = fixture(["", "one/alpha", "two/beta"]);
  const controller = combo.bind(f.root, f.document);
  f.query.input("one/alpha");
  f.options[1].dispatch("pointerdown", {pointerType: "mouse"});
  f.committed.value = "two/beta";
  assert.equal(controller.value(), "one/alpha");
  assert.deepEqual(controller.validate(), {valid: true, message: ""});
});

test("reset clears commitment and restores the workspace ARIA state", () => {
  const f = fixture(["", "one/alpha"]);
  const controller = combo.bind(f.root, f.document);
  f.query.input("one");
  f.options[1].dispatch("pointerdown");
  controller.reset();
  assert.equal(controller.value(), "");
  assert.equal(controller.query(), "");
  assert.equal(f.query.value, "");
  assert.equal(f.committed.value, "");
  assert.equal(f.query.getAttribute("aria-expanded"), "false");
  assert.deepEqual(f.selectedValues(), [""]);
});

test("pending closes and suppresses input, focus, keyboard, and pointer interaction", () => {
  const f = fixture(["", "one/alpha"]);
  const controller = combo.bind(f.root, f.document);
  f.query.input("one");
  controller.setPending(true);
  assert.equal(f.listbox.hidden, true);
  f.query.focus();
  f.query.keydown("ArrowDown");
  f.options[1].dispatch("pointerdown");
  assert.equal(controller.value(), "");
  assert.equal(f.listbox.hidden, true);
  controller.setPending(false);
  f.query.focus();
  assert.equal(f.listbox.hidden, false);
});

test("outside click closes while an inside pointerdown does not", () => {
  const f = fixture(["", "one/alpha"]);
  combo.bind(f.root, f.document);
  f.query.focus();
  f.document.dispatch("pointerdown", { target: f.query });
  assert.equal(f.listbox.hidden, false);
  f.document.dispatch("click", { target: new FakeTarget() });
  assert.equal(f.listbox.hidden, true);
});

test("destroy removes every listener and leaves subsequent events inert", () => {
  const f = fixture(["", "one/alpha"]);
  const controller = combo.bind(f.root, f.document);
  controller.destroy();
  f.query.focus();
  f.query.input("one");
  f.query.keydown("ArrowDown");
  f.options[1].dispatch("pointerdown");
  f.document.dispatch("pointerdown", { target: new FakeTarget() });
  f.document.dispatch("click", { target: new FakeTarget() });
  for (const target of [f.query, f.document, ...f.options]) {
    for (const listeners of target.listeners.values()) assert.equal(listeners.length, 0);
  }
  assert.equal(f.query.getAttribute("aria-expanded"), "false");
  assert.equal(controller.value(), "");
  assert.equal(f.committed.value, "");
});
