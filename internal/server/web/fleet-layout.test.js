"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const layout = require("./fleet-layout.js");

class FakeClassList {
  constructor() { this.names = new Set(); }
  add(...names) { names.forEach((name) => this.names.add(name)); }
  remove(...names) { names.forEach((name) => this.names.delete(name)); }
  contains(name) { return this.names.has(name); }
  toggle(name, force) {
    const enabled = force === undefined ? !this.names.has(name) : !!force;
    if (enabled) this.names.add(name); else this.names.delete(name);
    return enabled;
  }
}

class EventTarget {
  constructor() { this.listeners = new Map(); }
  addEventListener(type, listener) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(listener);
  }
  removeEventListener(type, listener) {
    this.listeners.set(type, (this.listeners.get(type) || []).filter((candidate) => candidate !== listener));
  }
  dispatch(type, extras = {}) {
    const event = {
      type,
      target: this,
      pointerId: undefined,
      defaultPrevented: false,
      preventDefault() { this.defaultPrevented = true; },
      ...extras
    };
    for (const listener of (this.listeners.get(type) || []).slice()) listener.call(this, event);
    return event;
  }
  listenerCount() {
    let count = 0;
    for (const listeners of this.listeners.values()) count += listeners.length;
    return count;
  }
}

class FakeElement extends EventTarget {
  constructor(id, tagName = "div") {
    super();
    this.id = id;
    this.tagName = tagName.toUpperCase();
    this.attributes = new Map();
    this.classList = new FakeClassList();
    this.style = {
      values: new Map(),
      setProperty: (name, value) => this.style.values.set(name, String(value)),
      removeProperty: (name) => this.style.values.delete(name),
      getPropertyValue: (name) => this.style.values.get(name) || ""
    };
    this.parentNode = null;
    this.captured = new Set();
  }
  setAttribute(name, value) { this.attributes.set(name, String(value)); }
  getAttribute(name) { return this.attributes.get(name) ?? null; }
  setPointerCapture(pointerId) { this.captured.add(pointerId); }
  hasPointerCapture(pointerId) { return this.captured.has(pointerId); }
  releasePointerCapture(pointerId) { this.captured.delete(pointerId); }
  closest(selector) {
    let node = this;
    while (node instanceof FakeElement) {
      if (selector === "#menuBtn" && node.id === "menuBtn") return node;
      if (selector === "#scrim" && node.id === "scrim") return node;
      if (selector === "[data-fleet-collapse]" && node.getAttribute("data-fleet-collapse") !== null) return node;
      node = node.parentNode;
    }
    return null;
  }
  click() {
    const event = {type: "click", target: this, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; }};
    for (const listener of (this.listeners.get("click") || []).slice()) listener.call(this, event);
    if (this.ownerDocument) this.ownerDocument.emit(event);
  }
}

class FakeDocument extends EventTarget {
  constructor() {
    super();
    this.elements = new Map();
    this.collapseButtons = [];
  }
  register(element) {
    this.elements.set(element.id, element);
    element.ownerDocument = this;
    return element;
  }
  getElementById(id) { return this.elements.get(id) || null; }
  querySelector(selector) {
    if (selector === ".app") return this.getElementById("app");
    return null;
  }
  querySelectorAll(selector) {
    return selector === "[data-fleet-collapse]" ? this.collapseButtons.slice() : [];
  }
  emit(event) {
    event.currentTarget = this;
    for (const listener of (this.listeners.get(event.type) || []).slice()) listener.call(this, event);
  }
}

class FakeMutationObserver {
  static instances = [];
  constructor(callback) { this.callback = callback; FakeMutationObserver.instances.push(this); }
  observe(target, options) { this.target = target; this.options = options; }
  disconnect() { this.disconnected = true; }
  static trigger(target) {
    for (const observer of FakeMutationObserver.instances) {
      if (!observer.disconnected && observer.target === target) observer.callback([{target}]);
    }
  }
}

function fakeStorage(initial = {}) {
  const values = new Map(Object.entries(initial));
  return {
    getItem(key) { return values.has(key) ? values.get(key) : null; },
    setItem(key, value) { values.set(key, String(value)); },
    removeItem(key) { values.delete(key); }
  };
}

function throwingStorage() {
  return {
    getItem() { throw new Error("storage unavailable"); },
    setItem() { throw new Error("storage unavailable"); }
  };
}

function fixture({innerWidth = 1400, storage = fakeStorage()} = {}) {
  FakeMutationObserver.instances = [];
  const document = new FakeDocument();
  const app = document.register(new FakeElement("app"));
  const fleetPanel = document.register(new FakeElement("fleet-panel"));
  const resizer = document.register(new FakeElement("fleet-resizer"));
  const mainStack = document.register(new FakeElement("main-stack"));
  const menu = document.register(new FakeElement("menuBtn", "button"));
  const scrim = document.register(new FakeElement("scrim"));
  const sidebar = document.register(new FakeElement("sidebar", "nav"));
  sidebar.parentNode = fleetPanel;
  const window = new EventTarget();
  window.innerWidth = innerWidth;
  window.localStorage = storage;
  window.MutationObserver = FakeMutationObserver;
  window.matchMedia = (query) => ({matches: query === "(max-width:820px)" ? window.innerWidth <= 820 : false});
  function installCollapseButton() {
    const button = new FakeElement("collapse-" + document.collapseButtons.length, "button");
    button.setAttribute("data-fleet-collapse", "");
    button.parentNode = fleetPanel;
    button.ownerDocument = document;
    document.collapseButtons.push(button);
    FakeMutationObserver.trigger(fleetPanel);
    return button;
  }
  function listenerCount() {
    return document.listenerCount() + window.listenerCount() + resizer.listenerCount();
  }
  return {document, window, storage, app, fleetPanel, resizer, mainStack, menu, scrim, sidebar, installCollapseButton, listenerCount};
}

function key(value) { return {key: value, preventDefault() { this.defaultPrevented = true; }}; }

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

test("invalid, empty, whitespace, or throwing storage falls back without aborting bind", () => {
  const f = fixture({innerWidth: 1400, storage: throwingStorage()});
  assert.doesNotThrow(() => layout.bind(f.document, f.window, f.storage));
  for (const saved of ["not-a-number", "", "   \t"] ) {
    const stored = fixture({innerWidth: 1400, storage: fakeStorage({
      "kanedias.fleet.width.v1": saved
    })});
    assert.equal(layout.bind(stored.document, stored.window, stored.storage).state().preferredWidth, 340);
  }
  const responsive = fixture({innerWidth: 900, storage: fakeStorage({
    "kanedias.fleet.width.v1": " "
  })});
  assert.equal(layout.bind(responsive.document, responsive.window, responsive.storage).state().preferredWidth, 300);
});

test("pointer and keyboard resizing clamp, persist, and synchronize ARIA", () => {
  const f = fixture({innerWidth: 1400});
  const controller = layout.bind(f.document, f.window, f.storage);
  f.resizer.dispatch("pointerdown", {pointerId: 7, clientX: 340});
  assert.equal(f.resizer.hasPointerCapture(7), true);
  f.window.dispatch("pointermove", {pointerId: 8, clientX: 500});
  assert.equal(controller.state().effectiveWidth, 340);
  f.window.dispatch("pointermove", {pointerId: 7, clientX: 460});
  f.window.dispatch("pointerup", {pointerId: 7, clientX: 460});
  assert.equal(controller.state().effectiveWidth, 460);
  assert.equal(f.resizer.hasPointerCapture(7), false);
  assert.equal(f.resizer.getAttribute("aria-valuenow"), "460");
  assert.equal(f.storage.getItem("kanedias.fleet.width.v1"), "460");
  f.resizer.dispatch("pointerdown", {pointerId: 9, clientX: 460});
  f.window.dispatch("pointermove", {pointerId: 9, clientX: 1000});
  f.window.dispatch("pointerup", {pointerId: 9, clientX: 1000});
  assert.equal(controller.state().preferredWidth, 560);
  assert.equal(f.storage.getItem("kanedias.fleet.width.v1"), "560");
  f.resizer.dispatch("keydown", key("Home"));
  assert.equal(controller.state().effectiveWidth, 240);
  f.resizer.dispatch("keydown", key("End"));
  assert.equal(controller.state().effectiveWidth, 560);
  f.resizer.dispatch("keydown", key("ArrowLeft"));
  assert.equal(controller.state().effectiveWidth, 544);
  f.resizer.dispatch("keydown", key("ArrowRight"));
  assert.equal(controller.state().effectiveWidth, 560);
});

test("patched collapse controls hide and restore through the stable top bar", () => {
  const f = fixture({innerWidth: 1400});
  const controller = layout.bind(f.document, f.window, f.storage);
  const collapse = f.installCollapseButton();
  assert.equal(collapse.getAttribute("aria-label"), "Hide Fleet");
  assert.equal(collapse.getAttribute("aria-controls"), "fleet-panel");
  assert.equal(collapse.getAttribute("aria-expanded"), "true");
  collapse.click();
  assert.equal(controller.state().collapsed, true);
  assert.equal(f.app.classList.contains("fleet-collapsed"), true);
  assert.equal(f.storage.getItem("kanedias.fleet.collapsed.v1"), "true");
  assert.equal(f.menu.getAttribute("aria-label"), "Show Fleet");
  assert.equal(collapse.getAttribute("aria-expanded"), "false");
  const patchedWhileCollapsed = f.installCollapseButton();
  assert.equal(patchedWhileCollapsed.getAttribute("aria-expanded"), "false");
  f.menu.click();
  assert.equal(controller.state().collapsed, false);
  assert.equal(f.storage.getItem("kanedias.fleet.collapsed.v1"), "false");
  assert.equal(collapse.getAttribute("aria-expanded"), "true");
  assert.equal(patchedWhileCollapsed.getAttribute("aria-expanded"), "true");
});

test("show restores a question-hidden Fleet without changing width", () => {
  const f = fixture({innerWidth: 1400, storage: fakeStorage({
    "kanedias.fleet.width.v1": "520",
    "kanedias.fleet.collapsed.v1": "true"
  })});
  const controller = layout.bind(f.document, f.window, f.storage);
  controller.show();
  assert.equal(controller.state().collapsed, false);
  assert.equal(controller.state().preferredWidth, 520);
  assert.equal(f.app.style.getPropertyValue("--fleet-width"), "520px");
});

test("mobile uses a non-persistent sheet and restores desktop preferences", () => {
  const f = fixture({innerWidth: 700});
  const controller = layout.bind(f.document, f.window, f.storage);
  f.menu.click();
  assert.equal(f.sidebar.classList.contains("open"), true);
  assert.equal(f.scrim.classList.contains("show"), true);
  assert.equal(f.menu.getAttribute("aria-expanded"), "true");
  assert.equal(f.storage.getItem("kanedias.fleet.collapsed.v1"), null);
  f.scrim.click();
  assert.equal(f.sidebar.classList.contains("open"), false);
  controller.show();
  f.window.innerWidth = 1400;
  f.window.dispatch("resize");
  assert.equal(f.sidebar.classList.contains("open"), false);
  assert.equal(f.scrim.classList.contains("show"), false);
  assert.equal(f.app.classList.contains("fleet-collapsed"), false);
  controller.destroy();
  controller.destroy();
  assert.equal(f.listenerCount(), 0);
  assert.equal(FakeMutationObserver.instances.every((observer) => observer.disconnected), true);
});

test("destroy cancels an active drag and beforeunload destroys idempotently", () => {
  const f = fixture({innerWidth: 1400});
  const controller = layout.bind(f.document, f.window, f.storage);
  f.resizer.dispatch("pointerdown", {pointerId: 4, clientX: 340});
  assert.equal(f.app.classList.contains("fleet-resizing"), true);
  f.window.dispatch("beforeunload");
  assert.equal(f.resizer.hasPointerCapture(4), false);
  assert.equal(f.app.classList.contains("fleet-resizing"), false);
  assert.equal(f.listenerCount(), 0);
  controller.destroy();
});
