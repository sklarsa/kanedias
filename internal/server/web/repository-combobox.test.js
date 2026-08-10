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
  keydown(key) { return this.dispatch("keydown", { key }); }
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
  root.parent = document;
  query.parent = root;
  committed.parent = root;
  listbox.parent = root;
  results.parent = root;
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
    ["[data-repository-results]", results]
  ]);
  root.querySelector = (selector) => bySelector.get(selector) || null;
  root.querySelectorAll = (selector) => selector === "[data-repository-option]" ? options : [];

  return {
    document, root, query, committed, listbox, results, options,
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

test("Arrow keys, Home, and End move the active option through filtered results", () => {
  const f = fixture(["", "one/alpha", "one/beta", "two/gamma"]);
  combo.bind(f.root, f.document);
  f.query.input("one/");
  f.query.keydown("ArrowDown");
  assert.equal(f.query.getAttribute("aria-activedescendant"), "repository-option-1");
  f.query.keydown("ArrowDown");
  assert.equal(f.query.getAttribute("aria-activedescendant"), "repository-option-2");
  f.query.keydown("ArrowUp");
  assert.equal(f.query.getAttribute("aria-activedescendant"), "repository-option-1");
  f.query.keydown("End");
  assert.equal(f.query.getAttribute("aria-activedescendant"), "repository-option-2");
  f.query.keydown("Home");
  assert.equal(f.query.getAttribute("aria-activedescendant"), "repository-option-1");
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

test("pointerdown commits before blur can close the listbox", () => {
  const f = fixture(["", "one/alpha"]);
  const controller = combo.bind(f.root, f.document);
  f.query.input("one");
  const pointer = f.options[1].dispatch("pointerdown");
  f.query.blur();
  assert.equal(pointer.defaultPrevented, true);
  assert.equal(controller.value(), "one/alpha");
  assert.equal(f.query.value, "one/alpha");
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
