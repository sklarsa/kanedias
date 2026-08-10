"use strict";

const test = require("node:test");
const assert = require("node:assert/strict");
const {spawnSync} = require("node:child_process");
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
    this.style = {};
    this.scrollHeight = 30;
    this.value = "";
    this.files = [];
    this.textContent = "";
  }
  set className(value) {
    this.classList.names = new Set(String(value).split(/\s+/).filter(Boolean));
  }
  get className() { return Array.from(this.classList.names).join(" "); }
  set value(value) {
    this._value = String(value);
    this.scrollHeight = this._value.split("\n").length * 30;
  }
  get value() { return this._value; }
  get firstChild() { return this.children[0] || null; }
  appendChild(child) { child.parentNode = this; child.ownerDocument = this.ownerDocument; this.children.push(child); return child; }
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
  focus() {
    this.focused = true;
    this.focusCount = (this.focusCount || 0) + 1;
    if (this.ownerDocument) this.ownerDocument.activeElement = this;
  }
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
    this.activeElement = null;
  }
  register(element) { this.elements.set(element.id, element); element.parentNode = this; element.ownerDocument = this; return element; }
  getElementById(id) { return this.elements.get(id) || null; }
  createElement(tagName) { const element = new FakeElement(tagName); element.ownerDocument = this; return element; }
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
  const bytes = new TextEncoder().encode(JSON.stringify(body));
  let read = false;
  return {
    status,
    headers: {get: (name) => name.toLowerCase() === "content-type" ? "application/json" : String(bytes.length)},
    body: {getReader: () => ({
      read: async () => read ? {done: true} : (read = true, {done: false, value: bytes}),
      cancel: async () => {}
    })}
  };
}

function image(name) { return {name, type: "image/png", size: 10, lastModified: 1}; }
function clipboardFile(file) { return {kind: "file", type: file.type, getAsFile: () => file}; }

function fixture(fetchImpl = async () => jsonResponse(202, {accepted: true}), options = {}) {
  FakeMutationObserver.instances = [];
  const document = new FakeDocument();
  if (options.fonts) document.fonts = options.fonts;
  const ids = [
    ["app", "div"], ["deck", "footer"], ["deck-input", "textarea"],
    ["image-attachment-tray", "div"], ["attach-images-button", "button"],
    ["image-file-input", "input"], ["steerBtn", "button"], ["deck-status", "div"],
    ["detail-panel", "div"], ["main-stack", "div"], ["fleet-panel", "div"],
    ["interruptBtn", "button"], ["stop", "button"]
  ];
  ids.forEach(([id, tag]) => document.register(new FakeElement(tag, id)));
  document.getElementById("deck").setAttribute("aria-busy", "false");
  document.getElementById("detail-panel").setAttribute("data-can-steer", "true");
  document.getElementById("detail-panel").setAttribute("data-session-id", "");
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
  const windowListeners = new Map();
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
    addEventListener(type, listener) {
      if (!windowListeners.has(type)) windowListeners.set(type, []);
      windowListeners.get(type).push(listener);
    },
    removeEventListener(type, listener) {
      windowListeners.set(type, (windowListeners.get(type) || []).filter((candidate) => candidate !== listener));
    },
    getComputedStyle() {
      if (options.onGetComputedStyle) options.onGetComputedStyle();
      return {
        lineHeight: "20px", paddingTop: "5px", paddingBottom: "5px",
        borderTopWidth: "1px", borderBottomWidth: "1px"
      };
    }
  };
  const binding = app.bindComposer(document, windowObject);
  const setCanSteer = (enabled, sessionID) => {
    const detail = document.getElementById("detail-panel");
    detail.setAttribute("data-can-steer", String(enabled));
    if (sessionID !== undefined) detail.setAttribute("data-session-id", sessionID);
    FakeMutationObserver.trigger(document.getElementById("main-stack"));
  };
  const select = (sessionID) => {
    rows.find((row) => row.dataset.sessionId === sessionID).click();
    setCanSteer(true, sessionID);
  };
  return {document, windowObject, windowListeners, binding, rows, select, setCanSteer, revoked};
}

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

test("text-only edits preserve preview nodes and focused removal moves to the adjacent control", () => {
  const f = fixture();
  f.select("A");
  changePicker(f, [image("one.png"), image("two.png"), image("three.png")]);
  const tray = f.document.getElementById("image-attachment-tray");
  const originalCards = tray.children.slice();
  const input = f.document.getElementById("deck-input");
  input.value = "typed";
  input.dispatchEvent(browserEvent("input"));
  assert.deepEqual(tray.children, originalCards, "directive changes rebuilt the aria-live tray");

  const removes = tray.querySelectorAll("[data-remove-image]");
  removes[1].focus();
  removes[1].click();
  const remaining = tray.querySelectorAll("[data-remove-image]");
  assert.equal(remaining.length, 2);
  assert.equal(f.document.activeElement, remaining[1], "focus did not move to the next remove control");

  remaining[1].focus();
  remaining[1].click();
  tray.querySelectorAll("[data-remove-image]")[0].focus();
  tray.querySelectorAll("[data-remove-image]")[0].click();
  assert.equal(f.document.activeElement, f.document.getElementById("attach-images-button"));
});

test("composer focus returns only when the same selected draft unlocks", async () => {
  const waiting = deferred();
  const f = fixture(() => waiting.promise);
  f.select("A");
  const input = f.document.getElementById("deck-input");
  input.value = "send A";
  input.dispatchEvent(browserEvent("input"));
  input.focus();
  const before = input.focusCount;
  f.document.getElementById("steerBtn").click();
  waiting.resolve(jsonResponse(202, {accepted: true}));
  await flush();
  assert.equal(input.focusCount, before + 1);

  const otherWaiting = deferred();
  const other = fixture(() => otherWaiting.promise);
  other.select("A");
  const otherInput = other.document.getElementById("deck-input");
  otherInput.value = "send A";
  otherInput.dispatchEvent(browserEvent("input"));
  otherInput.focus();
  const otherBefore = otherInput.focusCount;
  other.document.getElementById("steerBtn").click();
  other.select("B");
  otherWaiting.resolve(jsonResponse(202, {accepted: true}));
  await flush();
  assert.equal(otherInput.focusCount, otherBefore, "A completion stole focus while B was selected");
});

test("selection clears visible status and stale detail capabilities fail closed", () => {
  const f = fixture();
  f.select("A");
  f.document.getElementById("steerBtn").click();
  assert.match(f.document.getElementById("deck-status").textContent, /Enter a message/);
  f.select("B");
  assert.equal(f.document.getElementById("deck-status").textContent, "");

  f.setCanSteer(true, "A");
  assert.equal(f.binding.canEditSelectedDraft(), false);
  assert.equal(f.document.getElementById("deck-input").disabled, true);
  f.setCanSteer(true, "B");
  assert.equal(f.binding.canEditSelectedDraft(), true);
});

test("fleet reconciliation clears status when it removes the selected session", () => {
  const f = fixture();
  const status = f.document.getElementById("deck-status");
  f.select("A");
  f.document.getElementById("steerBtn").click();
  assert.equal(status.textContent, "Enter a message or attach an image.");

  f.document.rows = f.rows.filter((row) => row.dataset.sessionId !== "A");
  FakeMutationObserver.trigger(f.document.getElementById("fleet-panel"));
  assert.equal(status.textContent, "");
});

test("fleet reconciliation prunes status owned by a removed session", () => {
  const f = fixture();
  const status = f.document.getElementById("deck-status");
  f.select("A");
  f.document.getElementById("steerBtn").click();
  f.select("B");

  f.document.rows = f.rows.filter((row) => row.dataset.sessionId !== "A");
  FakeMutationObserver.trigger(f.document.getElementById("fleet-panel"));
  f.document.rows = f.rows.slice();
  FakeMutationObserver.trigger(f.document.getElementById("fleet-panel"));
  f.select("A");
  assert.equal(status.textContent, "");
});

test("capability revocation clears active drag affordance", () => {
  const f = fixture();
  f.select("A");
  const deck = f.document.getElementById("deck");
  deck.dispatchEvent(browserEvent("dragenter", {dataTransfer: {types: ["Files"], files: [image("drop.png")]}}));
  assert.equal(deck.classList.contains("drop-active"), true);
  f.setCanSteer(false, "A");
  assert.equal(deck.classList.contains("drop-active"), false);
});

test("clipboard image candidates with blank or case-varied declarations reach shared staging", () => {
  const f = fixture();
  f.select("A");
  const input = f.document.getElementById("deck-input");
  const paste = browserEvent("paste", {clipboardData: {items: [
    clipboardFile({...image("blank.png"), type: ""}),
    clipboardFile({...image("case.png"), type: "IMAGE/PNG"}),
    {kind: "string", type: "text/plain"}
  ]}});
  input.dispatchEvent(paste);
  assert.equal(paste.defaultPrevented, true);
  assert.deepEqual(f.binding.controller.draft("A").images.map((entry) => entry.name), ["blank.png", "case.png"]);
});

test("font readiness recalculates unless the composer binding was destroyed", async () => {
  const ready = deferred();
  const f = fixture(undefined, {fonts: {ready: ready.promise}});
  const input = f.document.getElementById("deck-input");
  f.select("A");
  input.scrollHeight = 220;
  ready.resolve();
  await flush();
  assert.equal(input.style.height, "132px");

  const lateReady = deferred();
  let recalculations = 0;
  const destroyed = fixture(undefined, {
    fonts: {ready: lateReady.promise},
    onGetComputedStyle() { recalculations++; }
  });
  destroyed.binding.destroy();
  const before = recalculations;
  lateReady.resolve();
  await flush();
  assert.equal(recalculations, before);
});

test("destroy removes binding listeners and permits a clean rebind", () => {
  const f = fixture();
  assert.equal((f.document.getElementById("steerBtn").listeners.get("click") || []).length, 1);
  assert.equal((f.windowListeners.get("beforeunload") || []).length, 1);
  assert.equal((f.windowListeners.get("resize") || []).length, 1);
  const input = f.document.getElementById("deck-input");
  input.scrollHeight = 220;
  for (const listener of f.windowListeners.get("resize") || []) listener();
  assert.equal(input.style.height, "132px");
  f.binding.destroy();
  f.binding.destroy();
  assert.equal((f.document.getElementById("steerBtn").listeners.get("click") || []).length, 0);
  assert.equal((f.windowListeners.get("beforeunload") || []).length, 0);
  assert.equal((f.windowListeners.get("resize") || []).length, 0);
  const rebound = app.bindComposer(f.document, f.windowObject);
  assert.equal((f.document.getElementById("steerBtn").listeners.get("click") || []).length, 1);
  rebound.destroy();
});

test("CommonJS require never executes full-page startup even with DOM globals", () => {
  const script = `
    global.window = {KanediasSessionModal:{bind(){throw new Error("startup ran")}}};
    global.document = {};
    require(${JSON.stringify(require.resolve("./app.js"))});
  `;
  const result = spawnSync(process.execPath, ["-e", script], {encoding: "utf8"});
  assert.equal(result.status, 0, result.stderr);
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
