"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const app = require("./app.js");
const terminalUI = require("./terminal-ui.js");
const imageAttachments = require("./image-attachments.js");

class FakeClassList {
  constructor(element) { this.element = element; this.names = new Set(); }
  add(...names) { names.forEach((name) => this.names.add(name)); }
  remove(...names) { names.forEach((name) => this.names.delete(name)); }
  contains(name) { return this.names.has(name); }
  toggle(name, force) {
    const enabled = force === undefined ? !this.names.has(name) : !!force;
    if (enabled) this.names.add(name); else this.names.delete(name);
    return enabled;
  }
}

class FakeElement {
  constructor(tagName = "div", id = "") {
    this.tagName = tagName.toUpperCase();
    this.id = id;
    this.parentNode = null;
    this.children = [];
    this.listeners = new Map();
    this.attributes = new Map();
    this.dataset = {};
    this.classList = new FakeClassList(this);
    this.disabled = false;
    this.hidden = false;
    this.value = "";
    this.files = [];
    this.textContent = "";
  }
  set className(value) {
    this.classList.names = new Set(String(value).split(/\s+/).filter(Boolean));
  }
  get className() { return Array.from(this.classList.names).join(" "); }
  get firstChild() { return this.children[0] || null; }
  appendChild(child) { child.parentNode = this; this.children.push(child); return child; }
  removeChild(child) {
    const index = this.children.indexOf(child);
    if (index >= 0) this.children.splice(index, 1);
    child.parentNode = null;
    return child;
  }
  setAttribute(name, value) {
    this.attributes.set(name, String(value));
    if (name.startsWith("data-")) {
      const key = name.slice(5).replace(/-([a-z])/g, (_, letter) => letter.toUpperCase());
      this.dataset[key] = String(value);
    }
  }
  getAttribute(name) { return this.attributes.get(name) ?? null; }
  addEventListener(type, listener) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(listener);
  }
  removeEventListener(type, listener) {
    const listeners = this.listeners.get(type) || [];
    this.listeners.set(type, listeners.filter((candidate) => candidate !== listener));
  }
  dispatchEvent(event) {
    if (!event.target) event.target = this;
    event.currentTarget = this;
    for (const listener of this.listeners.get(event.type) || []) listener.call(this, event);
    if (event.bubbles !== false && this.parentNode && this.parentNode.dispatchEvent) this.parentNode.dispatchEvent(event);
    return !event.defaultPrevented;
  }
  click() { this.dispatchEvent(browserEvent("click")); }
  focus() { this.focused = true; }
  setSelectionRange(start, end) { this.selectionStart = start; this.selectionEnd = end; }
  closest(selector) {
    let node = this;
    while (node && node instanceof FakeElement) {
      if (selector === "[data-remove-image]" && node.getAttribute("data-remove-image") !== null) return node;
      if (selector === ".row[data-session-id]" && node.classList.contains("row") && node.dataset.sessionId) return node;
      if (selector === ".row" && node.classList.contains("row")) return node;
      node = node.parentNode;
    }
    return null;
  }
  querySelectorAll(selector) {
    const found = [];
    function visit(node) {
      for (const child of node.children || []) {
        if (selector === "[data-remove-image]" && child.getAttribute("data-remove-image") !== null) found.push(child);
        visit(child);
      }
    }
    visit(this);
    return found;
  }
}

class FakeDocument {
  constructor() {
    this.elements = new Map();
    this.listeners = new Map();
    this.rows = [];
  }
  register(element) { this.elements.set(element.id, element); element.parentNode = this; return element; }
  getElementById(id) { return this.elements.get(id) || null; }
  createElement(tagName) { return new FakeElement(tagName); }
  addEventListener(type, listener) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(listener);
  }
  removeEventListener(type, listener) {
    this.listeners.set(type, (this.listeners.get(type) || []).filter((candidate) => candidate !== listener));
  }
  dispatchEvent(event) {
    event.currentTarget = this;
    for (const listener of this.listeners.get(event.type) || []) listener.call(this, event);
    return !event.defaultPrevented;
  }
  querySelector(selector) {
    const ids = {
      ".app": "app",
      ".deck": "deck",
      ".deck-input": "deck-input",
      ".dbtn.stop": "stop"
    };
    return ids[selector] ? this.getElementById(ids[selector]) : null;
  }
  querySelectorAll(selector) {
    if (selector === ".row[data-session-id]") return this.rows.slice();
    if (selector === "[data-remove-image]") return this.getElementById("image-attachment-tray").querySelectorAll(selector);
    return [];
  }
}

function browserEvent(type, extras = {}) {
  return {
    type,
    bubbles: true,
    defaultPrevented: false,
    preventDefault() { this.defaultPrevented = true; },
    ...extras
  };
}

class FakeMutationObserver {
  static instances = [];
  constructor(callback) { this.callback = callback; FakeMutationObserver.instances.push(this); }
  observe(target, options) { this.target = target; this.options = options; }
  disconnect() { this.disconnected = true; }
  static trigger(target) {
    for (const observer of FakeMutationObserver.instances) {
      if (!observer.disconnected && observer.target === target) observer.callback([{type: "attributes", target}]);
    }
  }
}

class FakeFormData {
  constructor() { this.parts = []; }
  append(name, value, filename) { this.parts.push({name, value, filename}); }
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return {promise, resolve, reject};
}

function jsonResponse(status, body) {
  return {
    status,
    headers: {get: () => "application/json"},
    json: async () => body
  };
}

function image(name) { return {name, type: "image/png", size: 10, lastModified: 1}; }
function clipboardFile(file) { return {kind: "file", type: file.type, getAsFile: () => file}; }

function fixture(fetchImpl = async () => jsonResponse(202, {accepted: true})) {
  FakeMutationObserver.instances = [];
  const document = new FakeDocument();
  const ids = [
    ["app", "div"], ["deck", "footer"], ["deck-input", "input"],
    ["image-attachment-tray", "div"], ["attach-images-button", "button"],
    ["image-file-input", "input"], ["steerBtn", "button"], ["deck-status", "div"],
    ["detail-panel", "div"], ["main-stack", "div"], ["fleet-panel", "div"],
    ["interruptBtn", "button"], ["stop", "button"]
  ];
  ids.forEach(([id, tag]) => document.register(new FakeElement(tag, id)));
  document.getElementById("deck").setAttribute("aria-busy", "false");
  document.getElementById("detail-panel").setAttribute("data-can-steer", "true");
  document.getElementById("detail-panel").setAttribute("data-can-interrupt", "true");
  document.getElementById("detail-panel").setAttribute("data-can-stop", "true");
  const rows = ["A", "B"].map((sessionID) => {
    const row = new FakeElement("div");
    row.classList.add("row");
    row.dataset.sessionId = sessionID;
    row.parentNode = document;
    return row;
  });
  document.rows = rows;
  const revoked = [];
  const windowObject = {
    KanediasTerminalUI: terminalUI,
    KanediasImageAttachments: imageAttachments,
    MutationObserver: FakeMutationObserver,
    FormData: FakeFormData,
    fetch: fetchImpl,
    URL: {
      createObjectURL: (file) => "blob:" + file.name + ":" + Math.random(),
      revokeObjectURL: (url) => revoked.push(url)
    },
    addEventListener() {},
    removeEventListener() {}
  };
  const binding = app.bindComposer(document, windowObject);
  const setCanSteer = (enabled) => {
    document.getElementById("detail-panel").setAttribute("data-can-steer", String(enabled));
    FakeMutationObserver.trigger(document.getElementById("main-stack"));
  };
  const select = (sessionID) => {
    rows.find((row) => row.dataset.sessionId === sessionID).click();
    setCanSteer(true);
  };
  return {document, windowObject, binding, rows, select, setCanSteer, revoked};
}

function changePicker(f, files) {
  const picker = f.document.getElementById("image-file-input");
  picker.files = files;
  picker.value = files.length ? "C:\\fakepath\\" + files[0].name : "";
  picker.dispatchEvent(browserEvent("change"));
  return picker;
}

function flush() { return new Promise((resolve) => setImmediate(resolve)); }

test("bound composer resets same-file picker and restores independent A/B drafts", () => {
  const f = fixture();
  const input = f.document.getElementById("deck-input");
  const same = image("same.png");

  f.select("A");
  input.value = "alpha";
  input.dispatchEvent(browserEvent("input"));
  assert.equal(changePicker(f, [same]).value, "");
  assert.equal(changePicker(f, [same]).value, "");
  assert.equal(f.binding.controller.draft("A").images.length, 2);

  f.select("B");
  assert.equal(input.value, "");
  input.value = "beta";
  input.dispatchEvent(browserEvent("input"));
  changePicker(f, [image("b.png")]);

  f.select("A");
  assert.equal(input.value, "alpha");
  assert.equal(f.document.getElementById("image-attachment-tray").children.length, 2);
  f.select("B");
  assert.equal(input.value, "beta");
  assert.equal(f.document.getElementById("image-attachment-tray").children.length, 1);
});

test("capability transitions disable remove and guard drop plus late picker change", () => {
  const f = fixture();
  f.select("A");
  changePicker(f, [image("kept.png")]);
  const tray = f.document.getElementById("image-attachment-tray");

  f.setCanSteer(false);
  const remove = tray.querySelectorAll("[data-remove-image]")[0];
  assert.equal(remove.disabled, true);
  remove.click();
  assert.equal(f.binding.controller.draft("A").images.length, 1);

  const picker = changePicker(f, [image("late.png")]);
  assert.equal(picker.value, "");
  assert.equal(f.binding.controller.draft("A").images.length, 1);

  const deck = f.document.getElementById("deck");
  deck.dispatchEvent(browserEvent("dragenter", {dataTransfer: {types: ["Files"], files: [image("drop.png")]}}));
  assert.equal(deck.classList.contains("drop-active"), false);
  const drop = browserEvent("drop", {dataTransfer: {types: ["Files"], files: [image("drop.png")]}});
  deck.dispatchEvent(drop);
  assert.equal(f.binding.controller.draft("A").images.length, 1);

  f.setCanSteer(true);
  assert.equal(tray.querySelectorAll("[data-remove-image]")[0].disabled, false);
  deck.dispatchEvent(browserEvent("dragenter", {dataTransfer: {types: ["Files"], files: [image("drop.png")]}}));
  assert.equal(deck.classList.contains("drop-active"), true);
  deck.dispatchEvent(browserEvent("drop", {
    dataTransfer: {types: ["Files"], files: [image("drop.png")]}
  }));
  assert.equal(f.binding.controller.draft("A").images.length, 2);
  tray.querySelectorAll("[data-remove-image]")[0].click();
  assert.deepEqual(f.binding.controller.draft("A").images.map((entry) => entry.name), ["drop.png"]);
});

test("bound paste preserves ordinary text and stages enabled image or mixed clipboard data", () => {
  const f = fixture();
  f.select("A");
  const input = f.document.getElementById("deck-input");

  const ordinary = browserEvent("paste", {clipboardData: {items: [{kind: "string", type: "text/plain"}]}});
  input.dispatchEvent(ordinary);
  assert.equal(ordinary.defaultPrevented, false);

  const pasted = image("paste.png");
  const imagePaste = browserEvent("paste", {clipboardData: {items: [clipboardFile(pasted)]}});
  input.dispatchEvent(imagePaste);
  assert.equal(imagePaste.defaultPrevented, true);

  const mixed = browserEvent("paste", {clipboardData: {items: [
    {kind: "string", type: "text/plain"}, clipboardFile(image("mixed.png"))
  ]}});
  input.dispatchEvent(mixed);
  assert.equal(mixed.defaultPrevented, true);
  assert.deepEqual(f.binding.controller.draft("A").images.map((entry) => entry.name), ["paste.png", "mixed.png"]);

  f.setCanSteer(false);
  const disabled = browserEvent("paste", {clipboardData: {items: [clipboardFile(image("blocked.png"))]}});
  input.dispatchEvent(disabled);
  assert.equal(disabled.defaultPrevented, false);
  assert.equal(f.binding.controller.draft("A").images.length, 2);
});

for (const outcome of [
  {name: "accepted", response: jsonResponse(202, {accepted: true})},
  {name: "rejected", response: jsonResponse(409, {accepted: false, error: "A rejected"})},
  {name: "unknown", response: {status: 202, headers: {get: () => "text/plain"}, json: async () => ({accepted: true})}}
]) {
  test("deferred A " + outcome.name + " status cannot erase or replace selected B status", async () => {
    const waiting = deferred();
    const f = fixture(() => waiting.promise);
    const input = f.document.getElementById("deck-input");
    const status = f.document.getElementById("deck-status");

    f.select("A");
    input.value = "send A";
    input.dispatchEvent(browserEvent("input"));
    f.document.getElementById("steerBtn").click();
    assert.equal(f.binding.controller.draft("A").busy, true);

    f.select("B");
    assert.equal(input.disabled, false);
    f.document.getElementById("steerBtn").click();
    assert.equal(status.textContent, "Enter a message or attach an image.");

    waiting.resolve(outcome.response);
    await flush();
    assert.equal(status.textContent, "Enter a message or attach an image.");
  });
}
